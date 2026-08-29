package dshacp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sglang's OpenAI-compatible streams diverge from DeepSeek's wire in one
// way that breaks dsh's translator: follow-up tool_calls delta chunks
// carry EXPLICIT null id/name fields where DeepSeek omits the keys, and
// the adapter's `!== undefined` merge lets those nulls overwrite the real
// values from the first chunk — every tool call then fails as
// `unknown tool ""` (verified live; docs/NOTES.md, and the same merge is
// still on dsh master). Until that's fixed upstream, each worker's LLM
// traffic runs through a local reverse proxy that deletes null id/type/
// function.name fields from tool_calls entries in SSE data chunks;
// everything else streams through unchanged.

// Transport dials LLM endpoints IPv4-first, walking the whole address
// list. LAN model hosts accumulate stale AAAA/hosts entries while the box
// has no global v6 route (seen live: ten dead IPv6 addresses listed ahead
// of the one working IPv4 — curl fell through, Go's default dial did
// not). Shared with the TUI's endpoint reachability probe.
var Transport = &http.Transport{
	Proxy:       http.ProxyFromEnvironment,
	DialContext: v4FirstDialContext,
}

func v4FirstDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ordered := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			ordered = append(ordered, ip)
		}
	}
	for _, ip := range ips {
		if ip.IP.To4() == nil {
			ordered = append(ordered, ip)
		}
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	var firstErr error
	for _, ip := range ordered {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no addresses for %s", host)
	}
	return nil, firstErr
}

// llmProxy is one worker's translating reverse proxy.
type llmProxy struct {
	srv      *http.Server
	ln       net.Listener
	upstream *url.URL
}

// startLLMProxy listens on a random localhost port, forwarding to the
// OpenAI-compatible endpoint base URL.
func startLLMProxy(endpoint string) (*llmProxy, error) {
	up, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("llm proxy: parsing endpoint %q: %w", endpoint, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("llm proxy: %w", err)
	}
	p := &llmProxy{ln: ln, upstream: up}
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the base URL workers should use in place of the endpoint.
func (p *llmProxy) URL() string {
	return "http://" + p.ln.Addr().String()
}

func (p *llmProxy) Close() { _ = p.srv.Close() }

func (p *llmProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	out := *r.URL
	out.Scheme = p.upstream.Scheme
	out.Host = p.upstream.Host
	out.Path = p.upstream.Path + r.URL.Path
	req, err := http.NewRequestWithContext(r.Context(), r.Method, out.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := Transport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// SSE: forward line by line, rewriting data chunks and flushing per
	// line so streaming latency survives the hop.
	w.Header().Del("Content-Length") // rewrites change the length
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			line = "data: " + scrubToolCallNulls(strings.TrimSpace(data))
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// scrubToolCallNulls deletes null id/type/function/function.name fields
// from tool_calls entries in one SSE chunk payload. Anything unparseable
// (including "[DONE]") passes through unchanged.
func scrubToolCallNulls(payload string) string {
	var chunk map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk["choices"] == nil {
		return payload
	}
	var choices []map[string]any
	if json.Unmarshal(chunk["choices"], &choices) != nil {
		return payload
	}
	changed := false
	for _, choice := range choices {
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, c := range calls {
			call, _ := c.(map[string]any)
			for _, key := range []string{"id", "type", "function"} {
				if v, present := call[key]; present && v == nil {
					delete(call, key)
					changed = true
				}
			}
			if fn, _ := call["function"].(map[string]any); fn != nil {
				for _, key := range []string{"name", "arguments"} {
					if v, present := fn[key]; present && v == nil {
						delete(fn, key)
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return payload
	}
	rewritten, err := json.Marshal(choices)
	if err != nil {
		return payload
	}
	chunk["choices"] = rewritten
	out, err := json.Marshal(chunk)
	if err != nil {
		return payload
	}
	return string(out)
}
