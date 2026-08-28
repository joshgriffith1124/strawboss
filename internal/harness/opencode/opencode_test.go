package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
)

// fixtureSID is the real session id inside the captured fixtures
// (testdata/*.json[l] and events.sse come from a live opencode 1.18.25 run
// against the GX10 — see docs/NOTES.md).
const fixtureSID = "ses_fb60449aeffeOeKJ878Yqn4aj6"

// fakeServer replays the captured fixtures over the routes the harness
// uses. statusPolls counts /session/status calls; the first `busyPolls`
// report busy so Result's polling loop is exercised.
type fakeServer struct {
	t           *testing.T
	busyPolls   int32
	statusPolls atomic.Int32
	prompts     atomic.Int32
	lastPrompt  atomic.Value // json body string
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Location.Directory == "" {
			f.t.Errorf("create session: bad body (err=%v, dir=%q)", err, body.Location.Directory)
		}
		w.Write([]byte(`{"data":{"id":"` + fixtureSID + `"}}`))
	})
	mux.HandleFunc("POST /session/"+fixtureSID+"/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(readJSON(r))
		f.lastPrompt.Store(string(b))
		f.prompts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		n := f.statusPolls.Add(1)
		if n <= int32(f.busyPolls) {
			w.Write([]byte(`{"` + fixtureSID + `":{"type":"busy"}}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /session/"+fixtureSID+"/message", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(f.t, "messages.json"))
	})
	mux.HandleFunc("GET /api/session/"+fixtureSID, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(f.t, "session_info.json"))
	})
	mux.HandleFunc("GET /global/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture(f.t, "events.sse"))
	})
	return mux
}

func readJSON(r *http.Request) map[string]any {
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	return m
}

func newHarness(t *testing.T, f *fakeServer) *Harness {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	mc := config.ModelConfig{Name: "qwen-coder", Endpoint: srv.URL, Model: "spark-a/qwen3.8-27b", Harness: "opencode"}
	h := New(mc, t.TempDir(), t.TempDir())
	h.PollInterval = time.Millisecond
	return h
}

func TestSpawn(t *testing.T) {
	f := &fakeServer{t: t}
	h := newHarness(t, f)

	id, err := h.Spawn(context.Background(), "do the thing", config.ModelConfig{Model: "spark-a/qwen3.8-27b"})
	if err != nil {
		t.Fatal(err)
	}
	if id != fixtureSID {
		t.Errorf("worker id = %q", id)
	}
	prompt, _ := f.lastPrompt.Load().(string)
	for _, want := range []string{`"providerID":"spark-a"`, `"modelID":"qwen3.8-27b"`, "do the thing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt body missing %s: %s", want, prompt)
		}
	}
}

func TestSpawnBadModelRef(t *testing.T) {
	f := &fakeServer{t: t}
	h := newHarness(t, f)
	if _, err := h.Spawn(context.Background(), "x", config.ModelConfig{Model: "no-slash"}); err == nil {
		t.Fatal("want error for model ref without provider/")
	}
	if f.prompts.Load() != 0 {
		t.Error("no prompt should have been sent")
	}
}

func TestStatus(t *testing.T) {
	t.Run("busy is running", func(t *testing.T) {
		h := newHarness(t, &fakeServer{t: t, busyPolls: 100})
		st, err := h.Status(context.Background(), fixtureSID)
		if err != nil {
			t.Fatal(err)
		}
		if st != harness.StatusRunning {
			t.Errorf("status = %q", st)
		}
	})
	t.Run("idle with clean transcript is done", func(t *testing.T) {
		h := newHarness(t, &fakeServer{t: t})
		st, err := h.Status(context.Background(), fixtureSID)
		if err != nil {
			t.Fatal(err)
		}
		if st != harness.StatusDone {
			t.Errorf("status = %q", st)
		}
	})
}

func TestFinishedStatusClassification(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want harness.Status
	}{
		{"no assistant yet", []Message{{Info: MessageInfo{Role: "user"}}}, harness.StatusQueued},
		{"assistant error", []Message{{Info: MessageInfo{Role: "assistant", Error: json.RawMessage(`{"name":"APIError"}`)}}}, harness.StatusFailed},
		{"tool error", []Message{{
			Info:  MessageInfo{Role: "assistant"},
			Parts: []Part{{Type: "tool", State: ToolState{Status: "error"}}},
		}}, harness.StatusFailed},
		{"clean", []Message{{Info: MessageInfo{Role: "assistant"}, Parts: []Part{{Type: "text", Text: "ok"}}}}, harness.StatusDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := finishedStatus(tt.msgs); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	h := newHarness(t, &fakeServer{t: t})
	u, err := h.Usage(context.Background(), fixtureSID)
	if err != nil {
		t.Fatal(err)
	}
	// session_info.json: input 480, output 198, reasoning 0, cache read 15744.
	if u.InputTokens != 480+15744 {
		t.Errorf("input = %d", u.InputTokens)
	}
	if u.OutputTokens != 198 {
		t.Errorf("output = %d", u.OutputTokens)
	}
}

func TestResult(t *testing.T) {
	f := &fakeServer{t: t, busyPolls: 3}
	h := newHarness(t, f)

	res, err := h.Result(context.Background(), fixtureSID)
	if err != nil {
		t.Fatal(err)
	}
	if f.statusPolls.Load() < 4 {
		t.Errorf("expected polling through busy states, got %d polls", f.statusPolls.Load())
	}
	if res.Status != harness.StatusDone {
		t.Errorf("status = %q", res.Status)
	}
	if !strings.Contains(res.Summary, "global-fixture-ok") {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(res.Summary) > maxSummaryBytes+10 {
		t.Errorf("summary too long: %d bytes", len(res.Summary))
	}
	// Full transcript landed at LogPath as JSONL, one message per line.
	b, err := os.ReadFile(res.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Errorf("log has %d lines, want 3", len(lines))
	}
	var first Message
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("log line not valid JSON: %v", err)
	}
	if first.Info.Role != "user" {
		t.Errorf("first log message role = %q", first.Info.Role)
	}
}

func TestEvents(t *testing.T) {
	h := newHarness(t, &fakeServer{t: t})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := h.Events(ctx, fixtureSID)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var toolEvents int
	for ev := range ch { // closes at session.idle in the fixture
		switch ev.Kind {
		case "text":
			text.WriteString(ev.Text)
		case "tool":
			toolEvents++
			if !strings.Contains(ev.Text, "bash") {
				t.Errorf("tool event = %q", ev.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "global-fixture-ok") {
		t.Errorf("streamed text = %q", text.String())
	}
	if toolEvents == 0 {
		t.Error("no tool events streamed")
	}
}

func TestSummarizeError(t *testing.T) {
	msgs := []Message{{
		Info: MessageInfo{Role: "assistant", Error: json.RawMessage(`{"name":"APIError","data":{"message":"boom"}}`)},
	}}
	s := summarize(msgs)
	if !strings.Contains(s, "APIError") || !strings.Contains(s, "boom") {
		t.Errorf("summary = %q", s)
	}
}
