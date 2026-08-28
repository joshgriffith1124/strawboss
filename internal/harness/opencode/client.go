package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is a thin HTTP client for the `opencode serve` API. It mixes the
// v1 surface (prompt_async, status, messages — the fire-and-forget path)
// with v2 session info (cumulative tokens); see docs/NOTES.md for why.
type Client struct {
	Base string // e.g. "http://127.0.0.1:4477"
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// CreateSession makes a new session rooted at dir.
func (c *Client) CreateSession(ctx context.Context, dir, title string) (string, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	// v1 create honors the server's cwd; the v2 location field pins dir.
	var out struct {
		ID string `json:"id"`
	}
	if dir != "" {
		v2 := map[string]any{"location": map[string]string{"directory": dir}}
		var wrapped struct {
			Data SessionInfo `json:"data"`
		}
		if err := c.do(ctx, "POST", "/api/session", v2, &wrapped); err != nil {
			return "", err
		}
		return wrapped.Data.ID, nil
	}
	if err := c.do(ctx, "POST", "/session", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// PromptAsync fires a task at a session without waiting: the v1
// fire-and-forget path that actually starts a run (the v2 prompt endpoint
// only queues input — see docs/NOTES.md).
func (c *Client) PromptAsync(ctx context.Context, sessionID, providerID, modelID, variant, text string) error {
	body := map[string]any{
		"model": map[string]string{"providerID": providerID, "modelID": modelID},
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	if variant != "" {
		body["variant"] = variant
	}
	return c.do(ctx, "POST", "/session/"+sessionID+"/prompt_async", body, nil)
}

// Status returns the busy/retry state of every non-idle session; sessions
// absent from the map are idle. dir MUST be the directory the sessions
// live in: the endpoint is project-scoped, and without the directory
// param sessions rooted elsewhere are silently absent (see docs/NOTES.md).
func (c *Client) Status(ctx context.Context, dir string) (map[string]SessionStatus, error) {
	path := "/session/status"
	if dir != "" {
		path += "?directory=" + url.QueryEscape(dir)
	}
	out := map[string]SessionStatus{}
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Messages returns a session's full transcript.
func (c *Client) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	var out []Message
	if err := c.do(ctx, "GET", "/session/"+sessionID+"/message", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionInfo returns the session record with cumulative token counts.
func (c *Client) SessionInfo(ctx context.Context, sessionID string) (SessionInfo, error) {
	var out struct {
		Data SessionInfo `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/session/"+sessionID, nil, &out); err != nil {
		return SessionInfo{}, err
	}
	return out.Data, nil
}

// Abort stops a running session.
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.do(ctx, "POST", "/session/"+sessionID+"/abort", map[string]any{}, nil)
}

// Events subscribes to the server's SSE feed and emits every parsed event
// until the context is cancelled or the stream ends. Callers filter by
// session id. A dropped stream closes the channel — displayed state, never
// a crash.
//
// The subscription uses /global/event, not /event: the instance bus at
// /event only carries sessions rooted in the server's own directory, while
// workers run wherever the delegate points them (see docs/NOTES.md). Global
// events arrive wrapped as {"directory","project","payload":<event>}.
func (c *Client) Events(ctx context.Context) (<-chan ServerEvent, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/global/event", nil)
	if err != nil {
		return nil, fmt.Errorf("building event request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("subscribing to %s/event: %w", c.Base, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("subscribing to %s/event: status %d", c.Base, resp.StatusCode)
	}

	ch := make(chan ServerEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			ev, ok := ParseEventLine(sc.Text())
			if !ok {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// ParseEventLine parses one SSE line from /global/event ("data: {…}" with
// the {directory, payload} envelope). ok is false for comments, blanks,
// and unparseable lines. Exported so recorded streams can be replayed
// through the same parser (M4 demo mode).
func ParseEventLine(line string) (ServerEvent, bool) {
	data, found := strings.CutPrefix(strings.TrimSpace(line), "data:")
	if !found {
		return ServerEvent{}, false
	}
	var wrapper struct {
		Directory string          `json:"directory"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &wrapper); err != nil || len(wrapper.Payload) == 0 {
		return ServerEvent{}, false
	}
	var ev ServerEvent
	if err := json.Unmarshal(wrapper.Payload, &ev); err != nil {
		return ServerEvent{}, false
	}
	ev.Directory = wrapper.Directory
	return ev, true
}
