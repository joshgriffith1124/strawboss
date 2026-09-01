package live

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness/opencode"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
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

	// The session id was persisted for resume, scoped to the driver's
	// working directory (empty here → the process cwd, both sides).
	if sid := LoadSession(stateDir, ""); sid != "35a53d5b-ee5c-4624-9f59-afb4d9e34f26" {
		t.Errorf("persisted session = %q", sid)
	}
}

// TestRegistryWatcher feeds a workers.jsonl incrementally and checks the
// emitted worker msgs (historical replay + live tail).
func TestRegistryWatcher(t *testing.T) {
	stateDir := t.TempDir()
	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}

	// Pre-existing history: w1 done before startup.
	w1, _ := reg.Allocate("ses_a", "qwen-coder", "old task", "/repo", 0)
	if err := reg.Finish(w1, "ses_a", "done", "all good", "/logs/a.jsonl", time.Second, 100, 0, 20); err != nil {
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
	w2, err := reg.Allocate("ses_b", "qwen-small", "new task", "/repo", 0)
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
		"qwen-coder (spark-a/qwen3.8-27b)", "qwen-small", "terse",
		// One denied git command must not convince the supervisor that
		// delegation is blocked — the prompt has to explain the allowlist.
		"ONLY the delegate command", "delegation still works",
		// Workers get a repo map and failures auto-escalate; the
		// supervisor must not spend tokens duplicating either.
		"repository map", "retries the task ONCE"} {
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
	w1, _ := oldReg.Allocate("ses_old", "qwen-coder", "ancient history", "/repo", 0)
	_ = oldReg.Finish(w1, "ses_old", "done", "old", "/l", time.Second, 1, 0, 1)

	newReg := &registry.Registry{Path: oldReg.Path, Run: "run-new"}
	w2, _ := newReg.Allocate("ses_new", "qwen-coder", "current work", "/repo", 0)

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
	w1, _ := reg.Allocate("ses_old", "m1", "the original task", "/repo", 0)
	if err := reg.Finish(w1, "ses_old", "failed", "boom", "/l", time.Second, 1, 0, 1); err != nil {
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

// TestDshWorkerTailAndRecovery: a dsh worker's registry row starts a
// session-log tailer that feeds transcript + usage msgs; with no finished
// event after turn/end (delegate gone), the row closes as recovered.
func TestDshWorkerTailAndRecovery(t *testing.T) {
	stateDir := t.TempDir()
	const sid = "ses_dsh_1"
	logDir := filepath.Join(stateDir, "dsh-sessions", "proj", sid)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "harness", "dshacp", "testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "session.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	w1, err := reg.Allocate(sid, "m-dsh", "the dsh task", "/repo", 12345)
	if err != nil {
		t.Fatal(err)
	}

	models := []config.ModelConfig{{Name: "m-dsh", Endpoint: "http://fake:1/v1", Model: "x", Harness: "dsh"}}
	o := New(&supervisor.Driver{}, models, stateDir)
	o.DshRecoverGrace = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)

	var sawTool, sawText, sawUsage bool
	drainUntil(t, o.Feed(), 10*time.Second, func(m tea.Msg) bool {
		switch v := m.(type) {
		case ui.WorkerEventMsg:
			if v.ID == w1 && v.Kind == "tool" {
				sawTool = true
			}
			if v.ID == w1 && v.Kind == "text" {
				sawText = true
			}
		case ui.WorkerUsageMsg:
			if v.ID == w1 && v.Output == 52 {
				sawUsage = true
			}
		case ui.WorkerUpsertMsg:
			return v.ID == w1 && v.Status == "done" && strings.Contains(v.Summary, "recovered")
		}
		return false
	})
	if !sawTool || !sawText || !sawUsage {
		t.Errorf("tool=%v text=%v usage=%v", sawTool, sawText, sawUsage)
	}
	if o.workerPID[w1] != 12345 {
		t.Errorf("pid = %d", o.workerPID[w1])
	}
}

// TestKillDshWorkerSignalsPid: killing a dsh worker SIGTERMs the recorded
// subprocess pid instead of calling any opencode API.
func TestKillDshWorkerSignalsPid(t *testing.T) {
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = sleeper.Wait(); close(done) }()
	defer sleeper.Process.Kill()

	models := []config.ModelConfig{{Name: "m-dsh", Endpoint: "http://fake:1/v1", Model: "x", Harness: "dsh"}}
	o := New(&supervisor.Driver{}, models, t.TempDir())
	o.mu.Lock()
	o.workerSession["w1"] = "ses_dsh"
	o.workerModel["w1"] = "m-dsh"
	o.workerPID["w1"] = sleeper.Process.Pid
	o.unfinished["w1"] = true
	o.mu.Unlock()

	o.OnKillWorker("w1")
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "killed")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("worker process not terminated")
	}
}

// TestNtfyPushOnLiveFailure: a live failed worker pushes to the ntfy
// topic; replayed history from before startup never does.
func TestNtfyPushOnLiveFailure(t *testing.T) {
	type push struct{ path, body, title string }
	got := make(chan push, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- push{r.URL.Path, string(b), r.Header.Get("Title")}
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	// History from before this orchestrator existed: must not push.
	w1, _ := reg.Allocate("ses_hist", "m", "old task", "/repo", 0)
	if err := reg.Finish(w1, "ses_hist", "failed", "ancient failure", "/l", time.Second, 1, 0, 1); err != nil {
		t.Fatal(err)
	}

	o := New(&supervisor.Driver{}, nil, stateDir)
	o.Notify = config.Notify{NtfyTopic: "sb-test", NtfyServer: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)

	// Wait for the replay, then a live failure.
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.WorkerUsageMsg)
		return ok && u.ID == w1
	})
	w2, _ := reg.Allocate("ses_live", "m", "new task", "/repo", 0)
	if err := reg.Finish(w2, "ses_live", "failed", "exploded\ndetails", "/l", time.Second, 1, 0, 1); err != nil {
		t.Fatal(err)
	}

	select {
	case p := <-got:
		if p.path != "/sb-test" || !strings.Contains(p.body, w2+" failed — exploded") || p.title == "" {
			t.Errorf("push = %+v", p)
		}
		if strings.Contains(p.body, "details") {
			t.Errorf("push leaked past the first line: %q", p.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no push for live failure")
	}
	select {
	case p := <-got:
		t.Errorf("unexpected extra push (replayed history?): %+v", p)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestOpenClawTwoWay: channel history is baseline (never commands); a new
// human message becomes a supervisor prompt and is echoed to the chat;
// bot messages are ignored; supervisor replies relay back out while the
// remote conversation is active and stop after local input resumes.
func TestOpenClawTwoWay(t *testing.T) {
	dir := t.TempDir()
	sends := filepath.Join(dir, "sends.log")
	served := filepath.Join(dir, "served")
	script := `#!/bin/sh
case "$*" in
  *" read "*"--after"*)
    if [ -f ` + served + ` ]; then echo '{"payload":{"messages":[]}}'; else
      touch ` + served + `
      echo '{"payload":{"messages":[{"id":"201","content":"bot chatter","author":{"bot":true,"username":"TheJarvis"}},{"id":"200","content":"kill w3 and retry it","author":{"bot":false,"username":"josh"}}]}}'
    fi;;
  *" read "*)
    echo '{"payload":{"messages":[{"id":"101","content":"old human message","author":{"bot":false,"username":"josh"}},{"id":"100","content":"old bot","author":{"bot":true,"username":"TheJarvis"}}]}}';;
  *" send "*)
    echo "$*" >> ` + sends + `
    echo '{"messageId":"1"}';;
esac
`
	bin := filepath.Join(dir, "openclaw")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	o := New(&supervisor.Driver{}, nil, t.TempDir())
	o.Notify = config.Notify{OpenClawTarget: "channel:1", OpenClawBin: bin, OpenClawTwoWay: true}
	o.OpenClawPollEvery = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.openclawLoop(ctx)

	// The injected prompt reaches the supervisor queue…
	select {
	case p := <-o.prompts:
		if !strings.Contains(p, "kill w3 and retry it") || !strings.Contains(p, "via discord") {
			t.Errorf("prompt = %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt injected")
	}
	// …and is echoed into the chat as a user message.
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.SupUserMsg)
		return ok && strings.Contains(u.Text, "[discord] kill w3")
	})
	if !o.remoteActive.Load() {
		t.Error("remoteActive not set")
	}

	// Supervisor replies relay to the channel while remote is active.
	o.observeSup([]tea.Msg{ui.SupTextDoneMsg{Text: "w3 killed and retried"}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(sends); err == nil && strings.Contains(string(b), "w3 killed and retried") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reply not relayed")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Local input ends the remote conversation: no further relays.
	o.OnPrompt("back at the keyboard")
	<-o.prompts
	o.observeSup([]tea.Msg{ui.SupTextDoneMsg{Text: "local-only reply"}})
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(sends); strings.Contains(string(b), "local-only reply") {
		t.Error("relay continued after local input")
	}
	// History was never injected as a command.
	select {
	case p := <-o.prompts:
		t.Errorf("unexpected prompt: %q", p)
	default:
	}
}

// TestSessionScopedPerProject: strawboss in project B must never resume
// project A's supervisor (seen live: a new directory picked up the old
// project's session and kept working on it).
func TestSessionScopedPerProject(t *testing.T) {
	stateDir := t.TempDir()
	dirA, dirB := t.TempDir(), t.TempDir()

	runA, err := RunID(stateDir, dirA, false)
	if err != nil {
		t.Fatal(err)
	}
	runB, err := RunID(stateDir, dirB, false)
	if err != nil {
		t.Fatal(err)
	}
	if runA == runB {
		t.Errorf("run ids shared across projects: %s", runA)
	}
	// Re-reading without rotate keeps each project's own id.
	if again, _ := RunID(stateDir, dirA, false); again != runA {
		t.Errorf("dirA run changed: %s vs %s", again, runA)
	}

	// A session persisted for dirA is invisible from dirB.
	slot := ProjectDir(stateDir, dirA)
	if err := os.WriteFile(filepath.Join(slot, "supervisor-session"), []byte("ses-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSession(stateDir, dirA); got != "ses-a" {
		t.Errorf("dirA session = %q", got)
	}
	if got := LoadSession(stateDir, dirB); got != "" {
		t.Errorf("dirB leaked dirA's session: %q", got)
	}
}

// TestDshModelProbeNotesNotLoaded: a reachable endpoint that does not
// serve a configured model must report "model not loaded", idle or not.
func TestDshModelProbeNotesNotLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"some-other-model","max_model_len":262144}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	// ds-hit first: msgs emit in config order and the drain terminates on
	// ds-miss.
	models := []config.ModelConfig{
		{Name: "ds-hit", Endpoint: srv.URL, Model: "some-other-model", Harness: "dsh"},
		{Name: "ds-miss", Endpoint: srv.URL, Model: "wanted-model", Harness: "dsh"},
	}
	o := New(&supervisor.Driver{}, models, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.pollWorkers(ctx)

	sawHit := false
	drainUntil(t, o.Feed(), 15*time.Second, func(m tea.Msg) bool {
		ms, ok := m.(ui.ModelStatMsg)
		if !ok {
			return false
		}
		if ms.Name == "ds-hit" {
			if ms.Note != "dsh" {
				t.Errorf("served model note = %q", ms.Note)
			}
			if ms.ContextWindow != 262144 {
				t.Errorf("context window = %d", ms.ContextWindow)
			}
			sawHit = true
		}
		return ms.Name == "ds-miss" && ms.Note == "model not loaded"
	})
	if !sawHit {
		t.Error("served model never reported")
	}
}

// TestBudgetGuard: warn at 80%, stop at the ceiling (marker written for
// delegate to refuse on), and a window-based stop lifts when the window
// recovers.
func TestBudgetGuard(t *testing.T) {
	stateDir := t.TempDir()
	o := New(&supervisor.Driver{Dir: "/proj"}, nil, stateDir)
	o.Budget = config.Budget{MaxCostUSD: 1.0, MaxPlan5h: 80}
	stop := BudgetStopFile(stateDir, "/proj")

	o.observeSup([]tea.Msg{ui.SupUsageMsg{CostUSD: 0.85}})
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "budget at 80%")
	})
	if _, err := os.Stat(stop); err == nil {
		t.Fatal("stop marker written on warning")
	}

	o.observeSup([]tea.Msg{ui.SupUsageMsg{CostUSD: 0.20}})
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "blocked")
	})
	b, err := os.ReadFile(stop)
	if err != nil || !strings.Contains(string(b), "ceiling") {
		t.Fatalf("stop marker: %q err %v", b, err)
	}

	// A cost stop never lifts on its own (cost cannot shrink)…
	o.observeSup([]tea.Msg{ui.SupRateLimitMsg{FiveHour: 0.10}})
	if _, err := os.Stat(stop); err != nil {
		t.Fatal("cost stop lifted by a rate-limit event")
	}

	// …but a pure window stop does.
	o2 := New(&supervisor.Driver{Dir: "/proj2"}, nil, stateDir)
	o2.Budget = config.Budget{MaxPlan5h: 80}
	stop2 := BudgetStopFile(stateDir, "/proj2")
	o2.observeSup([]tea.Msg{ui.SupRateLimitMsg{FiveHour: 0.85}})
	drainUntil(t, o2.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "blocked")
	})
	o2.observeSup([]tea.Msg{ui.SupRateLimitMsg{FiveHour: 0.40}})
	drainUntil(t, o2.Feed(), 5*time.Second, func(m tea.Msg) bool {
		tm, ok := m.(ui.ToastMsg)
		return ok && strings.Contains(tm.Text, "unblocked")
	})
	if _, err := os.Stat(stop2); err == nil {
		t.Fatal("window stop not lifted after recovery")
	}
}

// TestSessionHistoryAndSwitch: sessions append once each, list newest
// first with run tallies, and switching repoints the pointers, resets
// worker state, and replays the chosen run.
func TestSessionHistoryAndSwitch(t *testing.T) {
	stateDir := t.TempDir()
	proj := t.TempDir()
	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl"), Run: "run-1"}
	w1, _ := reg.Allocate("ses_w_old", "m", "old farkle task", "/repo", 0)
	_ = reg.Finish(w1, "ses_w_old", "done", "ok", "/l", time.Second, 1, 0, 1)
	reg2 := &registry.Registry{Path: reg.Path, Run: "run-2"}
	w2, _ := reg2.Allocate("ses_w_new", "m", "new task", "/repo", 0)
	_ = reg2.Finish(w2, "ses_w_new", "failed", "boom", "/l", time.Second, 1, 0, 1)

	o := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	o.RunID = "run-2"
	o.appendSessionHistory("ses-1", "run-1", "build the farkle game")
	o.appendSessionHistory("ses-2", "run-2", "continue the project")
	o.appendSessionHistory("ses-2", "run-2", "dup must not append")

	list := o.ListSessions()
	if len(list) != 2 {
		t.Fatalf("sessions = %+v", list)
	}
	if list[0].ID != "ses-2" || !list[0].Current || list[0].Failed != 1 {
		t.Errorf("newest = %+v", list[0])
	}
	if list[1].ID != "ses-1" || list[1].Current || list[1].Done != 1 || list[1].Label != "build the farkle game" {
		t.Errorf("oldest = %+v", list[1])
	}

	// Seed some current-run worker state, then switch to run-1.
	o.mu.Lock()
	o.unfinished[w2] = true
	o.workerSession[w2] = "ses_w_new"
	o.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.watchRegistry(ctx)
	o.SwitchSession("ses-1", "run-1")

	if got := o.Driver.SessionID(); got != "ses-1" {
		t.Errorf("driver session = %q", got)
	}
	if got := LoadSession(stateDir, proj); got != "ses-1" {
		t.Errorf("persisted session = %q", got)
	}
	if run, _ := RunID(stateDir, proj, false); run != "run-1" {
		t.Errorf("persisted run = %q", run)
	}
	if got := o.Driver.EnvVar("STRAWBOSS_RUN"); got != "run-1" {
		t.Errorf("STRAWBOSS_RUN = %q, want run-1", got)
	}
	o.mu.Lock()
	if len(o.unfinished) != 0 && o.unfinished[w2] {
		t.Error("old run's worker state survived the switch")
	}
	o.mu.Unlock()

	sawSwitch := false
	drainUntil(t, o.Feed(), 10*time.Second, func(m tea.Msg) bool {
		if _, ok := m.(ui.SessionSwitchedMsg); ok {
			sawSwitch = true
		}
		u, ok := m.(ui.WorkerUpsertMsg)
		return ok && u.ID == w1 && u.Status == "done" // run-1 replayed
	})
	if !sawSwitch {
		t.Error("no SessionSwitchedMsg")
	}
}

// TestSupUsagePersistsAcrossRestart: the worker side of the token panel
// replays the whole run; the supervisor side must too, or every TUI
// restart makes the local offload look better than it is.
func TestSupUsagePersistsAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	proj := t.TempDir()

	o1 := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	o1.RunID = "run-x"
	o1.observeSup([]tea.Msg{ui.SupUsageMsg{Input: 1000, Output: 400, CacheRead: 50000, CacheWrite: 200, CostUSD: 0.42, Turns: 3}})
	o1.observeSup([]tea.Msg{ui.SupUsageMsg{Input: 500, Output: 100, CostUSD: 0.08, Turns: 1}})

	// A fresh orchestrator (TUI restart) seeds the run's totals.
	o2 := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	o2.RunID = "run-x"
	o2.seedSupUsage()
	seed := drainUntil(t, o2.Feed(), 5*time.Second, func(m tea.Msg) bool {
		_, ok := m.(ui.SupUsageMsg)
		return ok
	})
	u := seed[len(seed)-1].(ui.SupUsageMsg)
	if u.Input != 1500 || u.Output != 500 || u.CacheRead != 50000 || u.Turns != 4 || u.CostUSD != 0.5 {
		t.Fatalf("seed = %+v", u)
	}
	// The budget guard resumes from the persisted cost.
	o2.mu.Lock()
	cost := o2.supCostTotal
	o2.mu.Unlock()
	if cost != 0.5 {
		t.Errorf("budget cost seed = %v", cost)
	}

	// A different run starts from zero (no seed emission).
	o3 := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	o3.RunID = "run-y"
	o3.seedSupUsage()
	select {
	case m := <-o3.Feed():
		t.Fatalf("unexpected seed for a fresh run: %#v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestNewSessionAndCtxSeed: the context footprint persists in the run
// ledger and seeds back on restart; NewSession clears the session
// pointer, mints a fresh run, lifts any budget stop, and announces the
// reset — the in-TUI equivalent of --new.
func TestNewSessionAndCtxSeed(t *testing.T) {
	stateDir := t.TempDir()
	proj := t.TempDir()
	o := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	run1, err := RunID(stateDir, proj, true)
	if err != nil {
		t.Fatal(err)
	}
	o.RunID = run1
	o.Driver.SetSessionID("ses-old")
	slot := ProjectDir(stateDir, proj)
	_ = os.MkdirAll(slot, 0o755)
	_ = os.WriteFile(filepath.Join(slot, "supervisor-session"), []byte("ses-old"), 0o644)
	_ = os.WriteFile(BudgetStopFile(stateDir, proj), []byte("ceiling\n"), 0o644)

	// Per-call context lands in the ledger when the turn commits…
	o.noteSupCtx(480_500)
	o.recordSupUsage(ui.SupUsageMsg{Input: 1000, CacheRead: 900_000, Turns: 1})

	// …and a rebuilt orchestrator (TUI restart) seeds it back to the UI,
	// before any prompt burns tokens.
	o2 := New(&supervisor.Driver{Dir: proj}, nil, stateDir)
	o2.RunID = run1
	o2.seedSupUsage()
	seeded := false
	drainUntil(t, o2.Feed(), 5*time.Second, func(m tea.Msg) bool {
		u, ok := m.(ui.SupUsageMsg)
		seeded = ok && u.Ctx == 480_500
		return ok
	})
	if !seeded {
		t.Error("seed msg missing the persisted ctx")
	}

	o.NewSession()
	if got := o.Driver.SessionID(); got != "" {
		t.Errorf("driver session = %q", got)
	}
	if got := LoadSession(stateDir, proj); got != "" {
		t.Errorf("persisted session = %q", got)
	}
	run2, _ := RunID(stateDir, proj, false)
	if run2 == run1 {
		t.Error("run id not rotated")
	}
	// The delegate inherits STRAWBOSS_RUN through the supervisor env; a
	// stale value strands every new worker in the old run, invisible to
	// the watcher.
	if got := o.Driver.EnvVar("STRAWBOSS_RUN"); got != run2 {
		t.Errorf("STRAWBOSS_RUN = %q, want %q", got, run2)
	}
	if _, err := os.Stat(BudgetStopFile(stateDir, proj)); err == nil {
		t.Error("budget stop survived the fresh session")
	}
	o.mu.Lock()
	if o.supTotals.Ctx != 0 || o.lastPrompt != "" {
		t.Errorf("stale state: ctx=%d lastPrompt=%q", o.supTotals.Ctx, o.lastPrompt)
	}
	o.mu.Unlock()
	drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		s, ok := m.(ui.SessionSwitchedMsg)
		return ok && s.ID == ""
	})
}
