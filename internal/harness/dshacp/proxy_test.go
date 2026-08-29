package dshacp

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream chunk shapes are verbatim from a live sglang capture
// (docs/NOTES.md): the first tool_calls delta names the call, follow-ups
// carry explicit nulls that must not survive the proxy.
const sglangChunks = `data: {"id":"c1","choices":[{"index":0,"delta":{"role":null,"content":null,"tool_calls":[{"id":"call_9","index":0,"type":"function","function":{"name":"write_file","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"role":null,"content":null,"tool_calls":[{"id":null,"index":0,"type":"function","function":{"name":null,"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}

data: [DONE]

`

func TestProxyScrubsToolCallNulls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sglangChunks))
	}))
	defer upstream.Close()

	p, err := startLLMProxy(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	resp, err := http.Post(p.URL()+"/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var dataLines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, line)
		}
	}
	if len(dataLines) != 3 {
		t.Fatalf("data lines = %d: %v", len(dataLines), dataLines)
	}
	if !strings.Contains(dataLines[0], `"name":"write_file"`) || !strings.Contains(dataLines[0], `"id":"call_9"`) {
		t.Errorf("first chunk mangled: %s", dataLines[0])
	}
	second := dataLines[1]
	for _, gone := range []string{`"id":null`, `"name":null`} {
		if strings.Contains(second, gone) {
			t.Errorf("null survived: %s", second)
		}
	}
	if !strings.Contains(second, `"arguments":"{\"a\":1}"`) {
		t.Errorf("arguments lost: %s", second)
	}
	if dataLines[2] != "data: [DONE]" {
		t.Errorf("sentinel mangled: %q", dataLines[2])
	}
}

func TestProxyPassesNonStreamThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(418)
		_, _ = w.Write([]byte(`{"object":"error","message":"teapot"}`))
	}))
	defer upstream.Close()
	p, err := startLLMProxy(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := http.Get(p.URL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 418 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}
