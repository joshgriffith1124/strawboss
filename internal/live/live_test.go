package live

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/config"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
	"strawboss/internal/supervisor"
	"strawboss/internal/ui"
)

// drainUntil pulls feed msgs until pred returns true or the timeout hits.
func drainUntil(t *testing.T, feed <-chan tea.Msg, timeout time.Duration, pred func(tea.Msg) bool) []tea.Msg {
	t.Helper()
	var got []tea.Msg
	deadline := time.After(timeout)
	for {
		select {
		case m := <-feed:
			got = append(got, m)
			if pred(m) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out; got %d msgs: %#v", len(got), got)
		}
	}
}

// TestSupervisorTurnThroughOrchestrator runs a full turn against a fake
// claude binary replaying the real captured fixture stream.
func TestSupervisorTurnThroughOrchestrator(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "supervisor", "testdata", "turn1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := fmt.Sprintf("#!/bin/sh\nread line\ncat %q\nread line2\n", fixture)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	o := New(&supervisor.Driver{Command: bin}, nil, stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.supervisorLoop(ctx)

	o.OnPrompt("hello")
	msgs := drainUntil(t, o.Feed(), 10*time.Second, func(m tea.Msg) bool {
		_, done := m.(ui.SupTurnDoneMsg)
		return done
	})

	var sawInit, sawText, sawUsage bool
	for _, m := range msgs {
		switch v := m.(type) {
		case ui.SupInitMsg:
			sawInit = true
			if v.Auth != "subscription" {
				t.Errorf("auth = %q", v.Auth)
			}
			if v.PID == 0 {
				t.Error("pid = 0")
			}
		case ui.SupTextDoneMsg:
			if strings.Contains(v.Text, "strawboss spike ok") {
				sawText = true
			}
		case ui.SupUsageMsg:
			if v.CostUSD > 0 {
				sawUsage = true
			}
		}
	}
	if !sawInit || !sawText || !sawUsage {
		t.Errorf("init=%v text=%v usage=%v", sawInit, sawText, sawUsage)
	}

	// The session id was persisted for resume.
	if sid := LoadSession(stateDir); sid != "35a53d5b-ee5c-4624-9f59-afb4d9e34f26" {
		t.Errorf("persisted session = %q", sid)
	}
}

// TestRegistryWatcher feeds a workers.jsonl incrementally and checks the
// emitted worker msgs (historical replay + live tail).
func TestRegistryWatcher(t *testing.T) {
	stateDir := t.TempDir()
	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}

	// Pre-existing history: w1 done before startup.
	w1, _ := reg.Allocate("ses_a", "qwen-coder", "old task", "/repo")
	if err := reg.Finish(w1, "ses_a", "done", "all good", "/logs/a.jsonl", time.Second, 100, 20); err != nil {
		t.Fatal(err)
	}

	o := New(&supervisor.Driver{}, nil, stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)

	msgs := drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUsageMsg)
		return ok && u.ID == w1
	})
	first := msgs[0].(ui.WorkerUpsertMsg)
	if first.ID != w1 || first.Status != "running" || first.Task != "old task" {
		t.Errorf("first = %+v", first)
	}

	// Live tail: a new delegation lands while watching.
	w2, err := reg.Allocate("ses_b", "qwen-small", "new task", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUpsertMsg)
		if ok && u.ID == w2 {
			if u.Model != "qwen-small" || u.Status != "running" {
				t.Errorf("w2 = %+v", u)
			}
			return true
		}
		return false
	})

	// The watcher registered the session mapping for the event stream.
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sessionToWorker["ses_b"] != w2 || !o.unfinished[w2] {
		t.Errorf("mappings = %+v unfinished = %+v", o.sessionToWorker, o.unfinished)
	}
	if o.unfinished[w1] {
		t.Error("w1 should be finished")
	}
}

func TestMapWorkerEventFiltersUnknownSessions(t *testing.T) {
	o := New(&supervisor.Driver{}, nil, t.TempDir())
	o.sessionToWorker["ses_known"] = "w7"

	mk := func(sid, field, delta string) []tea.Msg {
		return o.mapWorkerEvent(sseDelta(sid, field, delta))
	}

	if msgs := mk("ses_unknown", "text", "x"); msgs != nil {
		t.Errorf("unknown session leaked: %#v", msgs)
	}
	msgs := mk("ses_known", "text", "hello")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	we := msgs[0].(ui.WorkerEventMsg)
	if we.ID != "w7" || we.Kind != "text" || we.Text != "hello" {
		t.Errorf("event = %+v", we)
	}
	if msgs := mk("ses_known", "somethingelse", "x"); msgs != nil {
		t.Errorf("non-text delta leaked: %#v", msgs)
	}
}

// sseDelta builds a message.part.delta ServerEvent for tests.
func sseDelta(sid, field, delta string) (ev opencode.ServerEvent) {
	ev.Type = "message.part.delta"
	ev.Properties.SessionID = sid
	ev.Properties.Field = field
	ev.Properties.Delta = delta
	return ev
}

func TestBuildSystemPrompt(t *testing.T) {
	p := BuildSystemPrompt("/usr/local/bin/strawboss", []config.ModelConfig{
		{Name: "qwen-coder", Model: "spark-a/qwen3.8-27b"},
		{Name: "qwen-small", Model: "spark-a/qwen3.8-flash-next"},
	})
	for _, want := range []string{"/usr/local/bin/strawboss delegate", "--model", "--task",
		"qwen-coder (spark-a/qwen3.8-27b)", "qwen-small", "terse"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestShutdownKillsEverything: exiting must abort unfinished workers and
// terminate the in-flight supervisor turn.
func TestShutdownKillsEverything(t *testing.T) {
	var aborts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session/{sid}/abort", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sid") == "ses_live" {
			aborts.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A claude that emits init then sleeps until signalled.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"s9\",\"apiKeySource\":\"none\"}'\nexec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	models := []config.ModelConfig{{Name: "m1", Endpoint: srv.URL, Model: "p/x", Harness: "opencode"}}
	o := New(&supervisor.Driver{Command: bin}, models, stateDir)
	o.Run(context.Background())

	// One unfinished worker on record, one turn in flight.
	o.mu.Lock()
	o.workerSession["w1"] = "ses_live"
	o.workerModel["w1"] = "m1"
	o.unfinished["w1"] = true
	o.mu.Unlock()
	o.OnPrompt("hello")
	drainUntil(t, o.Feed(), 10*time.Second, func(m tea.Msg) bool {
		_, ok := m.(ui.SupInitMsg)
		return ok
	})

	done := make(chan struct{})
	go func() { o.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if aborts.Load() != 1 {
		t.Errorf("aborts = %d, want 1", aborts.Load())
	}
	// The turn's claude process must be gone, reported as a requested stop
	// (a live process would still be sleeping).
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		d, ok := m.(ui.SupTurnDoneMsg)
		return ok && d.Err == "" && d.Interrupted
	})
}

// TestRegistryWatcherScopesByRun: only the current run's workers replay;
// other runs' history stays out of a fresh session.
func TestRegistryWatcherScopesByRun(t *testing.T) {
	stateDir := t.TempDir()
	oldReg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl"), Run: "run-old"}
	w1, _ := oldReg.Allocate("ses_old", "qwen-coder", "ancient history", "/repo")
	_ = oldReg.Finish(w1, "ses_old", "done", "old", "/l", time.Second, 1, 1)

	newReg := &registry.Registry{Path: oldReg.Path, Run: "run-new"}
	w2, _ := newReg.Allocate("ses_new", "qwen-coder", "current work", "/repo")

	o := New(&supervisor.Driver{}, nil, stateDir)
	o.RunID = "run-new"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)

	msgs := drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUpsertMsg)
		return ok && u.ID == w2
	})
	for _, m := range msgs {
		if u, ok := m.(ui.WorkerUpsertMsg); ok && u.ID == w1 {
			t.Errorf("old run's worker leaked into the feed: %+v", u)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unfinished[w1] {
		t.Error("old run's worker tracked as unfinished")
	}
}

// TestKillWorkerAbortsSession: dashboard `x` aborts the worker's opencode
// session; finished workers refuse.
func TestKillWorkerAbortsSession(t *testing.T) {
	var aborts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session/{sid}/abort", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sid") == "ses_live" {
			aborts.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	models := []config.ModelConfig{{Name: "m1", Endpoint: srv.URL, Model: "p/x", Harness: "opencode"}}
	o := New(&supervisor.Driver{}, models, t.TempDir())
	o.mu.Lock()
	o.workerSession["w1"] = "ses_live"
	o.workerModel["w1"] = "m1"
	o.unfinished["w1"] = true
	o.mu.Unlock()

	o.OnKillWorker("w1")
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "killed")
	})
	if aborts.Load() != 1 {
		t.Errorf("aborts = %d, want 1", aborts.Load())
	}

	// A finished worker has nothing to kill.
	o.mu.Lock()
	delete(o.unfinished, "w1")
	o.mu.Unlock()
	o.OnKillWorker("w1")
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "nothing to kill")
	})
	if aborts.Load() != 1 {
		t.Errorf("aborts = %d after refusal, want still 1", aborts.Load())
	}
}

// TestRetryWorkerSpawnsNewWorker: dashboard `r` re-runs a finished worker's
// task as a new worker through the harness + registry; the watcher flows
// the new row into the feed.
func TestRetryWorkerSpawnsNewWorker(t *testing.T) {
	var prompts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"ses_retry"}}`)
	})
	mux.HandleFunc("POST /session/{sid}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		prompts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /session/{sid}/message", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"info":{"id":"msg_1","sessionID":"ses_retry","role":"assistant","time":{"created":1,"completed":2}},"parts":[{"type":"text","text":"did it right this time"}]}]`))
	})
	mux.HandleFunc("GET /api/session/{sid}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"id":"ses_retry","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":0,"write":0}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stateDir := t.TempDir()
	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	w1, _ := reg.Allocate("ses_old", "m1", "the original task", "/repo")
	if err := reg.Finish(w1, "ses_old", "failed", "boom", "/l", time.Second, 1, 1); err != nil {
		t.Fatal(err)
	}

	models := []config.ModelConfig{{Name: "m1", Endpoint: srv.URL, Model: "p/x", Harness: "opencode"}}
	o := New(&supervisor.Driver{}, models, stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)

	// History replay populates the task/model/dir maps retry reads.
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUsageMsg)
		return ok && u.ID == w1
	})

	o.OnRetryWorker(w1)
	msgs := drainUntil(t, o.Feed(), 10*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUpsertMsg)
		return ok && u.ID == "w2" && u.Status == "done"
	})
	for _, m := range msgs {
		if u, ok := m.(ui.WorkerUpsertMsg); ok && u.ID == "w2" && u.Task != "" && u.Task != "the original task" {
			t.Errorf("retry task = %q", u.Task)
		}
	}
	if prompts.Load() != 1 {
		t.Errorf("prompts = %d, want 1", prompts.Load())
	}

	// A still-running worker refuses to retry.
	o.mu.Lock()
	o.workerTask["w9"] = "t"
	o.unfinished["w9"] = true
	o.mu.Unlock()
	o.OnRetryWorker("w9")
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "still running")
	})
}
