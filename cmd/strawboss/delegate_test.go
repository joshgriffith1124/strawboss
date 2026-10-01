package main

import (
	"context"
	"flag"
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

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/live"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/slots"
)

// fakeOpencode serves the minimal API surface delegate exercises. mode
// selects the worker outcome: "done", "failed", or "hang" (never finishes).
type fakeOpencode struct {
	mode       string
	aborts     atomic.Int32
	created    atomic.Int32
	lastPrompt atomic.Value // raw prompt_async request body
}

const testSID = "ses_delegate_test_1"

func (f *fakeOpencode) handler() http.Handler {
	assistant := func(sid, errField string) string {
		return fmt.Sprintf(`{
			"info":{"id":"msg_2","sessionID":%[1]q,"role":"assistant","time":{"created":1,"completed":2}%[2]s},
			"parts":[{"type":"text","text":"summary: did the thing"}]
		}`, sid, errField)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		n := f.created.Add(1)
		fmt.Fprintf(w, `{"data":{"id":"ses_delegate_test_%d"}}`, n)
	})
	mux.HandleFunc("POST /session/{sid}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.lastPrompt.Store(string(body))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /session/{sid}/abort", func(w http.ResponseWriter, r *http.Request) {
		f.aborts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /session/status", func(w http.ResponseWriter, r *http.Request) {
		if f.mode == "hang" {
			fmt.Fprintf(w, `{"ses_delegate_test_1":{"type":"busy"},"ses_delegate_test_2":{"type":"busy"}}`)
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /session/{sid}/message", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		errField := ""
		if f.mode == "failed" {
			errField = `,"error":{"name":"UnknownError","data":{"message":"it broke"}}`
		}
		w.Write([]byte(`[{"info":{"id":"msg_1","sessionID":"` + sid + `","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"the task"}]},` + assistant(sid, errField) + `]`))
	})
	mux.HandleFunc("GET /api/session/{sid}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"id":"` + r.PathValue("sid") + `","tokens":{"input":500,"output":90,"reasoning":0,"cache":{"read":1500,"write":0}}}}`))
	})
	return mux
}

// setup writes a models.toml pointing at the fake server and returns
// delegate args plus the state dir.
func setup(t *testing.T, f *fakeOpencode) (stateDir string, baseArgs []string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	stateDir = t.TempDir()
	models := filepath.Join(stateDir, "models.toml")
	toml := "[models.qwen-coder]\nendpoint = \"" + srv.URL + "\"\nmodel = \"spark-a/qwen3.8-27b\"\n"
	if err := os.WriteFile(models, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return stateDir, []string{"--state-dir", stateDir, "--dir", t.TempDir(), "--model", "qwen-coder"}
}

func TestDelegateDone(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)
	t.Setenv("STRAWBOSS_RUN", "run-env")

	var out strings.Builder
	err := runDelegate(append(args, "--task", "build the thing"), &out)
	if err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}

	got := out.String()
	if !strings.HasPrefix(got, "w1 done ") {
		t.Errorf("output = %q", got)
	}
	if !strings.Contains(got, "summary: did the thing") {
		t.Errorf("output missing summary: %q", got)
	}
	logPath := filepath.Join(stateDir, "logs", testSID+".jsonl")
	if !strings.Contains(got, logPath) {
		t.Errorf("output missing log path %s: %q", logPath, got)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("transcript log: %v", err)
	}

	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	events, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	workers := registry.Reduce(events)
	if len(workers) != 1 {
		t.Fatalf("workers = %+v", workers)
	}
	w := workers[0]
	if w.ID != "w1" || w.Status != "done" || w.Session != testSID {
		t.Errorf("worker = %+v", w)
	}
	for _, ev := range events {
		if ev.Run != "run-env" {
			t.Errorf("event run = %q, want run-env (from $STRAWBOSS_RUN)", ev.Run)
		}
	}
	if w.Task != "build the thing" || w.Model != "qwen-coder" {
		t.Errorf("worker = %+v", w)
	}
	// fake session: input 500, cache read 1500, output 90 — fresh and
	// cache recorded separately.
	if w.InputTokens != 500 || w.CacheReadTokens != 1500 || w.OutputTokens != 90 {
		t.Errorf("tokens = %d/%d/%d", w.InputTokens, w.CacheReadTokens, w.OutputTokens)
	}
}

func TestDelegateFailedWorker(t *testing.T) {
	f := &fakeOpencode{mode: "failed"}
	stateDir, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--task", "break the thing"), &out)
	if err == nil {
		t.Fatal("want error for failed worker")
	}
	if !strings.Contains(err.Error(), "1 of 1 workers failed") {
		t.Errorf("err = %v", err)
	}
	// The terse result still prints — the supervisor needs it either way.
	got := out.String()
	if !strings.HasPrefix(got, "w1 failed ") {
		t.Errorf("output = %q", got)
	}
	if !strings.Contains(got, "UnknownError") || !strings.Contains(got, "it broke") {
		t.Errorf("output = %q", got)
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].Status != "failed" {
		t.Errorf("workers = %+v", workers)
	}
}

func TestDelegateTimeoutAborts(t *testing.T) {
	f := &fakeOpencode{mode: "hang"}
	stateDir, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--task", "never finishes", "--timeout", "300ms"), &out)
	if err == nil {
		t.Fatal("want error on timeout")
	}
	if f.aborts.Load() == 0 {
		t.Error("worker was not aborted")
	}
	got := out.String()
	if !strings.HasPrefix(got, "w1 failed ") || !strings.Contains(got, "aborted") {
		t.Errorf("output = %q", got)
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].Status != "failed" {
		t.Errorf("workers = %+v", workers)
	}
}

func TestDelegateBadArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing task", []string{"--model", "x"}, "at least one --task"},
		// No --model is no longer an error: it routes each task with auto.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runDelegate(tt.args, &strings.Builder{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestDelegateUnknownModel(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	_, args := setup(t, f)
	err := runDelegate(append(args[:len(args)-1], "nope", "--task", "x"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), `no model "nope"`) {
		t.Errorf("err = %v", err)
	}
}

func TestDelegateParallelTasks(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--task", "task alpha", "--task", "task beta"), &out)
	if err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	got := out.String()
	// One terse block per worker, in task order.
	if !strings.Contains(got, "w1 done ") || !strings.Contains(got, "w2 done ") {
		t.Errorf("output = %q", got)
	}
	if strings.Count(got, "summary: did the thing") != 2 {
		t.Errorf("output = %q", got)
	}
	if f.created.Load() != 2 {
		t.Errorf("sessions created = %d", f.created.Load())
	}

	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 2 {
		t.Fatalf("workers = %+v", workers)
	}
	tasks := map[string]bool{}
	for _, w := range workers {
		if w.Status != "done" {
			t.Errorf("worker = %+v", w)
		}
		tasks[w.Task] = true
	}
	if !tasks["task alpha"] || !tasks["task beta"] {
		t.Errorf("tasks = %v", tasks)
	}
}

// TestDelegateDsh runs a delegation end-to-end over the dsh harness
// against a fake dsh-acp-demo bin, checking the terse result and that the
// registry recorded the worker subprocess pid for TUI kill.
func TestDelegateDsh(t *testing.T) {
	dir := t.TempDir()
	fixture, err := filepath.Abs(filepath.Join("..", "..", "internal", "harness", "dshacp", "testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "ses_dsh_e2e"
	script := `#!/bin/sh
mkdir -p "$STRAWBOSS_DSH_SESSIONS/proj/` + sid + `"
cp "` + fixture + `" "$STRAWBOSS_DSH_SESSIONS/proj/` + sid + `/session.jsonl"
while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}';;
    *'"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"` + sid + `"}}';;
    *'"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sid + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"dsh worker all done."}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}';;
  esac
done
`
	bin := filepath.Join(dir, "dsh-acp-demo")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cordis.yml")
	if err := os.WriteFile(cfg, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRAWBOSS_DSH_BIN", bin)
	t.Setenv("STRAWBOSS_DSH_CONFIG", cfg)

	stateDir := t.TempDir()
	models := filepath.Join(stateDir, "models.toml")
	toml := "[models.ds-worker]\nendpoint = \"http://fake:1/v1\"\nmodel = \"fake-model\"\nharness = \"dsh\"\n"
	if err := os.WriteFile(models, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = runDelegate([]string{"--state-dir", stateDir, "--dir", t.TempDir(),
		"--model", "ds-worker", "--task", "do the dsh thing"}, &out)
	if err != nil {
		t.Fatalf("delegate: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"done", "dsh worker all done."} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	events, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	sawSpawn := false
	for _, ev := range events {
		if ev.Type == "spawned" {
			sawSpawn = true
			if ev.PID <= 0 {
				t.Errorf("spawned event pid = %d, want the dsh subprocess pid", ev.PID)
			}
		}
	}
	if !sawSpawn {
		t.Error("no spawned event recorded")
	}
}

// TestDelegateWorktree: with --worktree the worker runs in an isolated
// git worktree, its output is committed on a strawboss/* branch named in
// the terse result, and the main checkout stays untouched.
func TestDelegateWorktree(t *testing.T) {
	// A repo with one commit as the shared working dir.
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	// A fake dsh bin whose "worker" actually writes a file into its cwd
	// (the worktree) before finishing.
	fixture, err := filepath.Abs(filepath.Join("..", "..", "internal", "harness", "dshacp", "testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "ses_wt_e2e"
	binDir := t.TempDir()
	script := `#!/bin/sh
mkdir -p "$STRAWBOSS_DSH_SESSIONS/proj/` + sid + `"
cp "` + fixture + `" "$STRAWBOSS_DSH_SESSIONS/proj/` + sid + `/session.jsonl"
echo "made by worker" > worker-output.txt
while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}';;
    *'"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"` + sid + `"}}';;
    *'"session/prompt"'*)
      echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sid + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"wrote worker-output.txt"}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}';;
  esac
done
`
	bin := filepath.Join(binDir, "dsh-acp-demo")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(binDir, "cordis.yml")
	if err := os.WriteFile(cfg, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRAWBOSS_DSH_BIN", bin)
	t.Setenv("STRAWBOSS_DSH_CONFIG", cfg)

	stateDir := t.TempDir()
	models := filepath.Join(stateDir, "models.toml")
	toml := "[models.ds]\nendpoint = \"http://fake:1/v1\"\nmodel = \"m\"\nharness = \"dsh\"\n"
	if err := os.WriteFile(models, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err = runDelegate([]string{"--state-dir", stateDir, "--dir", repo,
		"--worktree", "--model", "ds", "--task", "write worker-output.txt"}, &out)
	if err != nil {
		t.Fatalf("delegate: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "work committed on branch strawboss/") {
		t.Errorf("result missing branch note:\n%s", got)
	}
	// Main checkout untouched; branch carries the file.
	if _, err := os.Stat(filepath.Join(repo, "worker-output.txt")); !os.IsNotExist(err) {
		t.Error("worker output leaked into the main checkout")
	}
	branches := exec.Command("git", "branch", "--list", "strawboss/*", "--contains")
	branches.Dir = repo
	bout, _ := branches.CombinedOutput()
	if !strings.Contains(string(bout), "strawboss/") {
		t.Errorf("no strawboss branch: %s", bout)
	}

	// A second run whose worker writes nothing removes its worktree.
	noWrite := strings.Replace(script, "echo \"made by worker\" > worker-output.txt\n", "", 1)
	if err := os.WriteFile(bin, []byte(noWrite), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = runDelegate([]string{"--state-dir", stateDir, "--dir", repo,
		"--worktree", "--model", "ds", "--task", "do nothing"}, &out)
	if err != nil {
		t.Fatalf("delegate: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no file changes; worktree removed") {
		t.Errorf("result missing removal note:\n%s", out.String())
	}
}

// TestDelegateRefusesOnBudgetStop: the stop marker turns delegation into
// a terse refusal before anything spawns.
func TestDelegateRefusesOnBudgetStop(t *testing.T) {
	stateDir := t.TempDir()
	dir := t.TempDir()
	stop := live.BudgetStopFile(stateDir, dir)
	if err := os.MkdirAll(filepath.Dir(stop), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stop, []byte("notional cost $5.00 reached the $5.00 ceiling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runDelegate([]string{"--state-dir", stateDir, "--dir", dir,
		"--model", "anything", "--task", "t"}, &out)
	if err == nil {
		t.Fatal("delegate did not refuse")
	}
	got := out.String()
	for _, want := range []string{"blocked by the budget guard", "$5.00 ceiling", "Do NOT retry"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal missing %q:\n%s", want, got)
		}
	}
}

// TestDelegateRefusesRepeatedFailedTask: the classic supervisor loop —
// resubmitting the exact task that already failed twice — is refused
// before anything spawns.
func TestDelegateRefusesRepeatedFailedTask(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)
	t.Setenv("STRAWBOSS_RUN", "run-loop")

	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl"), Run: "run-loop"}
	for i := 0; i < 2; i++ {
		wid, err := reg.Allocate(fmt.Sprintf("ses_prev_%d", i), "qwen-coder", "build the doomed thing", "/repo", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Finish(wid, "", "failed", "boom", "/l", 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	var out strings.Builder
	err := runDelegate(append(args, "--task", "build the doomed thing"), &out)
	if err == nil {
		t.Fatal("refused task should count as failed")
	}
	got := out.String()
	if !strings.Contains(got, "refused: this exact task has already failed 2 times") {
		t.Errorf("output missing refusal:\n%s", got)
	}
	if f.created.Load() != 0 {
		t.Errorf("worker spawned despite refusal: %d", f.created.Load())
	}

	// A different task on the same model still runs.
	out.Reset()
	if err := runDelegate(append(args, "--task", "a different task"), &out); err != nil {
		t.Fatalf("different task refused: %v\n%s", err, out.String())
	}
}

// setupCapped is setup with max_concurrent = 1 on qwen-coder, so the test
// itself can hold the model's only slot and force the delegate's worker to
// queue.
func setupCapped(t *testing.T, f *fakeOpencode) (stateDir string, baseArgs []string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	stateDir = t.TempDir()
	models := filepath.Join(stateDir, "models.toml")
	toml := "[models.qwen-coder]\nendpoint = \"" + srv.URL + "\"\nmodel = \"spark-a/qwen3.8-27b\"\nmax_concurrent = 1\n"
	if err := os.WriteFile(models, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return stateDir, []string{"--state-dir", stateDir, "--dir", t.TempDir(), "--model", "qwen-coder"}
}

// TestDelegateQueuesAtCap: with the model's only slot held, the worker is
// registered as queued, then spawns on the same id once the slot frees.
func TestDelegateQueuesAtCap(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setupCapped(t, f)

	// Hold the model's only slot so the delegate's worker must queue.
	release, ok, err := slots.TryAcquire(filepath.Join(stateDir, "slots"), "qwen-coder", 1)
	if err != nil {
		t.Fatalf("slot acquire: %v", err)
	}
	if !ok {
		t.Fatal("model's only slot not available for the test itself")
	}
	oldPoll := slots.PollEvery
	slots.PollEvery = 20 * time.Millisecond
	t.Cleanup(func() { slots.PollEvery = oldPoll })
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	var out strings.Builder
	if err := runDelegate(append(args, "--task", "build the thing"), &out); err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	if !strings.HasPrefix(out.String(), "w1 done ") {
		t.Errorf("output = %q", out.String())
	}

	reg := &registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}
	events, err := reg.Load()
	if err != nil {
		t.Fatal(err)
	}
	queuedIdx, spawnedIdx := -1, -1
	for i, ev := range events {
		if ev.Worker != "w1" {
			continue
		}
		if ev.Type == "queued" && queuedIdx < 0 {
			queuedIdx = i
		}
		if ev.Type == "spawned" && spawnedIdx < 0 {
			spawnedIdx = i
		}
	}
	if queuedIdx < 0 || spawnedIdx < 0 || queuedIdx >= spawnedIdx {
		t.Fatalf("want queued event for w1 before its spawned event: %+v", events)
	}
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].ID != "w1" || workers[0].Status != "done" {
		t.Errorf("workers = %+v", workers)
	}
}

// TestDelegateQueuedTimeout: the slot never frees, so the worker times out
// while still queued — no session is created, and the queued row is closed
// as failed.
func TestDelegateQueuedTimeout(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setupCapped(t, f)

	// Hold the model's only slot for the whole test.
	release, ok, err := slots.TryAcquire(filepath.Join(stateDir, "slots"), "qwen-coder", 1)
	if err != nil {
		t.Fatalf("slot acquire: %v", err)
	}
	if !ok {
		t.Fatal("model's only slot not available for the test itself")
	}
	t.Cleanup(release)
	oldPoll := slots.PollEvery
	slots.PollEvery = 20 * time.Millisecond
	t.Cleanup(func() { slots.PollEvery = oldPoll })

	var out strings.Builder
	err = runDelegate(append(args, "--timeout", "300ms", "--task", "build the thing"), &out)
	if err == nil {
		t.Fatalf("want error, out = %s", out.String())
	}
	if !strings.Contains(out.String(), "aborted while queued") {
		t.Errorf("output = %q", out.String())
	}

	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].Status != "failed" {
		t.Errorf("workers = %+v", workers)
	}
	if f.created.Load() != 0 {
		t.Errorf("sessions created = %d, want 0 (worker never spawned)", f.created.Load())
	}
}

// gitRepoDir makes a --dir that is a git repo with one tracked file, so
// the repo map has something to say.
func gitRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "lock.go"), []byte("package p\n\nfunc SlotLock() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("git", "add", "-A")
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	return dir
}

// TestDelegateEscalation: a failure on the requested model re-dispatches
// once to the NEXT config in models.toml, inside the same call — one
// terse result telling the whole story, both attempts in the registry.
func TestDelegateEscalation(t *testing.T) {
	cheap := &fakeOpencode{mode: "failed"}
	strong := &fakeOpencode{mode: "done"}
	srvCheap := httptest.NewServer(cheap.handler())
	srvStrong := httptest.NewServer(strong.handler())
	t.Cleanup(srvCheap.Close)
	t.Cleanup(srvStrong.Close)
	stateDir := t.TempDir()
	toml := "[models.cheap]\nendpoint = \"" + srvCheap.URL + "\"\nmodel = \"p/cheap\"\n" +
		"[models.strong]\nendpoint = \"" + srvStrong.URL + "\"\nmodel = \"p/strong\"\n"
	if err := os.WriteFile(filepath.Join(stateDir, "models.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--state-dir", stateDir, "--dir", t.TempDir(), "--model", "cheap", "--task", "fix the thing"}

	var out strings.Builder
	if err := runDelegate(args, &out); err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "w2 done ") {
		t.Errorf("output = %q", got)
	}
	if !strings.Contains(got, "[escalated from cheap, which failed:") || !strings.Contains(got, "it broke") {
		t.Errorf("escalation note missing: %q", got)
	}
	// Exactly one terse result — the supervisor never sees two.
	if strings.Contains(got, "w1 failed") {
		t.Errorf("first attempt leaked into supervisor output: %q", got)
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 2 {
		t.Fatalf("workers = %+v", workers)
	}
	byID := map[string]registry.Worker{}
	for _, w := range workers {
		byID[w.ID] = w
	}
	if byID["w1"].Status != "failed" || byID["w1"].Model != "cheap" {
		t.Errorf("w1 = %+v", byID["w1"])
	}
	if byID["w2"].Status != "done" || byID["w2"].Model != "strong" {
		t.Errorf("w2 = %+v", byID["w2"])
	}

	// --escalate=false keeps the old single-attempt behavior.
	cheap2 := &fakeOpencode{mode: "failed"}
	srv2 := httptest.NewServer(cheap2.handler())
	t.Cleanup(srv2.Close)
	state2 := t.TempDir()
	toml2 := "[models.cheap]\nendpoint = \"" + srv2.URL + "\"\nmodel = \"p/cheap\"\n" +
		"[models.strong]\nendpoint = \"" + srv2.URL + "\"\nmodel = \"p/strong\"\n"
	if err := os.WriteFile(filepath.Join(state2, "models.toml"), []byte(toml2), 0o644); err != nil {
		t.Fatal(err)
	}
	var out2 strings.Builder
	err := runDelegate([]string{"--state-dir", state2, "--dir", t.TempDir(),
		"--model", "cheap", "--escalate=false", "--task", "fix the thing"}, &out2)
	if err == nil || !strings.Contains(out2.String(), "w1 failed") {
		t.Errorf("escalate=false: err=%v out=%q", err, out2.String())
	}
}

// TestDelegatePromptGetsRepoMap: workers receive map + task; the
// registry records the bare task (loop-guard identity, TUI display).
func TestDelegatePromptGetsRepoMap(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)
	repo := gitRepoDir(t)
	// setup's --dir is args[3]; override with the git repo.
	for i, a := range args {
		if a == "--dir" {
			args[i+1] = repo
		}
	}
	var out strings.Builder
	if err := runDelegate(append(args, "--task", "add tests"), &out); err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	body, _ := f.lastPrompt.Load().(string)
	if !strings.Contains(body, "Repository map") || !strings.Contains(body, "lock.go: SlotLock") {
		t.Errorf("worker prompt missing repo map: %q", body)
	}
	if !strings.Contains(body, "Your task:") || !strings.Contains(body, "add tests") {
		t.Errorf("worker prompt missing task: %q", body)
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	for _, ev := range events {
		if ev.Type == "spawned" && ev.Task != "add tests" {
			t.Errorf("registry task = %q, want bare task", ev.Task)
		}
	}

	// --repomap=false sends the bare task.
	f2 := &fakeOpencode{mode: "done"}
	_, args2 := setup(t, f2)
	for i, a := range args2 {
		if a == "--dir" {
			args2[i+1] = repo
		}
	}
	var out2 strings.Builder
	if err := runDelegate(append(args2, "--repomap=false", "--task", "add tests"), &out2); err != nil {
		t.Fatal(err)
	}
	if body, _ := f2.lastPrompt.Load().(string); strings.Contains(body, "Repository map") {
		t.Errorf("--repomap=false still injected the map: %q", body)
	}
}

// TestTaskFileFlag: task prose containing $( ), backticks, or < > reads to
// Claude Code's Bash allowlist as a compound command and gets the whole
// delegate call denied — three real denials in the transcripts. A file
// path carries none of that, so --task-file must accept exactly what
// --task would have.
func TestTaskFileFlag(t *testing.T) {
	dir := t.TempDir()
	hostile := "REPLACE index.html with <style> blocks and a <script src=x> tag; run $(date) first"
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("  "+hostile+"\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var tasks []string
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	registerTaskFlags(fs, &tasks)
	if err := fs.Parse([]string{"--task", "inline one", "--task-file", path}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0] != "inline one" {
		t.Errorf("inline task = %q", tasks[0])
	}
	if tasks[1] != hostile {
		t.Errorf("file task = %q, want the trimmed file body", tasks[1])
	}
}

func TestTaskFileErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, want string
	}{
		{"missing file", filepath.Join(dir, "nope.md"), "reading task file"},
		{"empty file", empty, "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readTaskFile(tt.path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestPinFor: --model is sticky for the tasks after it; tasks before the
// first --model bind to it; no --model at all means auto.
func TestPinFor(t *testing.T) {
	tests := []struct {
		name string
		pins []modelPin
		want []string // per task index
	}{
		{"no model flag routes automatically", nil, []string{"auto", "auto"}},
		{"one model covers every task", []modelPin{{"big", 0}}, []string{"big", "big"}},
		{"task before the first model binds to it", []modelPin{{"big", 1}}, []string{"big", "big"}},
		{"sticky per group", []modelPin{{"fast", 0}, {"big", 2}}, []string{"fast", "fast", "big"}},
		{"auto mixes with pins", []modelPin{{"auto", 0}, {"big", 1}}, []string{"auto", "big"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, want := range tt.want {
				if got := pinFor(tt.pins, i); got != want {
					t.Errorf("task %d: pinFor = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestRouteTasks: auto tasks land on the tier their weight maps to, pinned
// tasks on the model named, and either routes around a model that is down
// — with a note only when the supervisor needs to know.
func TestRouteTasks(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "fast", Tier: 1},
		{Name: "spark-a", Tier: 2},
		{Name: "spark-b", Tier: 2},
	}
	light := "rename foo to bar in main.go"
	heavy := "refactor and debug the parser across a.go, b.go and c.go; investigate the root cause"
	availFn := func(down ...string) func(context.Context, config.ModelConfig) (bool, string) {
		return func(_ context.Context, mc config.ModelConfig) (bool, string) {
			for _, d := range down {
				if d == mc.Name {
					return false, "endpoint unreachable"
				}
			}
			return true, ""
		}
	}
	tests := []struct {
		name     string
		task     string
		pins     []modelPin
		down     []string
		want     string
		wantNote string // substring; "" = no note at all
	}{
		{"auto light task starts on the fast tier", light, nil, nil, "fast", "[auto → fast"},
		{"auto heavy task starts on the big tier", heavy, nil, nil, "spark-a", "[auto → spark-a"},
		{"auto routes around a dead fast model", light, nil, []string{"fast"}, "spark-a", "[fast unavailable (endpoint unreachable); ran on spark-a]"},
		{"pinned and available says nothing", light, []modelPin{{"spark-a", 0}}, nil, "spark-a", ""},
		{"pinned but down falls to its same-tier sibling", light, []modelPin{{"spark-a", 0}}, []string{"spark-a"}, "spark-b", "ran on spark-b"},
		{"nothing available keeps the want so the real error surfaces", light, []modelPin{{"fast", 0}}, []string{"fast", "spark-a", "spark-b"}, "fast", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes, err := routeTasks(models, []string{tt.task}, tt.pins, "models.toml", availFn(tt.down...), nil)
			if err != nil {
				t.Fatal(err)
			}
			if routes[0].mc.Name != tt.want {
				t.Errorf("model = %q, want %q", routes[0].mc.Name, tt.want)
			}
			if tt.wantNote == "" && routes[0].note != "" {
				t.Errorf("note = %q, want none", routes[0].note)
			}
			if !strings.Contains(routes[0].note, tt.wantNote) {
				t.Errorf("note = %q, want containing %q", routes[0].note, tt.wantNote)
			}
		})
	}

	if _, err := routeTasks(models, []string{light}, []modelPin{{"nope", 0}}, "models.toml", availFn(), nil); err == nil ||
		!strings.Contains(err.Error(), `no model "nope"`) {
		t.Errorf("unknown model: err = %v", err)
	}

	// Each model is probed at most once per call, however many tasks.
	probes := 0
	counting := func(context.Context, config.ModelConfig) (bool, string) { probes++; return true, "" }
	if _, err := routeTasks(models, []string{light, light, light}, nil, "models.toml", counting, nil); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (memoized per model)", probes)
	}
}

// TestRouteTasksFreeSlot: a pick at its max_concurrent cap routes to the
// first free same-tier sibling instead of queueing; tasks inside the same
// call count against the cap too.
func TestRouteTasksFreeSlot(t *testing.T) {
	availFn := func(_ context.Context, mc config.ModelConfig) (bool, string) { return true, "" }

	t.Run("saturated pinned model routes to a free sibling", func(t *testing.T) {
		models := []config.ModelConfig{
			{Name: "a", Tier: 1, MaxConcurrent: 1},
			{Name: "b", Tier: 1},
		}
		busy := func(m config.ModelConfig) int {
			if m.Name == "a" {
				return 1
			}
			return 0
		}
		routes, err := routeTasks(models, []string{"task"}, []modelPin{{"a", 0}}, "models.toml", availFn, busy)
		if err != nil {
			t.Fatal(err)
		}
		if routes[0].mc.Name != "b" {
			t.Errorf("model = %q, want b", routes[0].mc.Name)
		}
		if !strings.Contains(routes[0].note, "slots full") {
			t.Errorf("note = %q, want it containing slots full", routes[0].note)
		}
	})

	t.Run("tasks in the same call count against the cap", func(t *testing.T) {
		models := []config.ModelConfig{
			{Name: "a", Tier: 1, MaxConcurrent: 2},
			{Name: "b", Tier: 1},
		}
		routes, err := routeTasks(models, []string{"t1", "t2", "t3"}, []modelPin{{"a", 0}}, "models.toml", availFn, nil)
		if err != nil {
			t.Fatal(err)
		}
		if routes[0].mc.Name != "a" || routes[1].mc.Name != "a" {
			t.Errorf("first two = %q, %q, want a, a", routes[0].mc.Name, routes[1].mc.Name)
		}
		if routes[2].mc.Name != "b" {
			t.Errorf("third = %q, want b", routes[2].mc.Name)
		}
		if !strings.Contains(routes[2].note, "slots full") {
			t.Errorf("third note = %q, want it containing slots full", routes[2].note)
		}
	})
}

// TestRouteTasksPool: pool members share work evenly — a pinned task on a
// pool member lands on the least-loaded member (file order breaking ties),
// and auto routing through a pool alternates instead of filling the first
// member. Models without a pool keep fill-first behavior.
func TestRouteTasksPool(t *testing.T) {
	availFn := func(_ context.Context, mc config.ModelConfig) (bool, string) { return true, "" }
	poolAB := []config.ModelConfig{
		{Name: "a", Tier: 1, Pool: "qwen", MaxConcurrent: 2},
		{Name: "b", Tier: 1, Pool: "qwen", MaxConcurrent: 2},
	}

	t.Run("pinned tasks alternate across the pool", func(t *testing.T) {
		routes, err := routeTasks(poolAB, []string{"t1", "t2", "t3", "t4"}, []modelPin{{"a", 0}}, "models.toml", availFn, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := routes[0].mc.Name + routes[1].mc.Name + routes[2].mc.Name + routes[3].mc.Name
		if got != "abab" {
			t.Errorf("models = %q, want abab", got)
		}
		if !strings.Contains(routes[1].note, "ran on b (pool qwen)") {
			t.Errorf("note = %q, want it naming the pool member", routes[1].note)
		}
	})

	t.Run("busy on one member sheds to the idle one", func(t *testing.T) {
		busy := func(m config.ModelConfig) int {
			if m.Name == "a" {
				return 1
			}
			return 0
		}
		routes, err := routeTasks(poolAB, []string{"t1"}, []modelPin{{"a", 0}}, "models.toml", availFn, busy)
		if err != nil {
			t.Fatal(err)
		}
		if routes[0].mc.Name != "b" {
			t.Errorf("model = %q, want b", routes[0].mc.Name)
		}
	})

	t.Run("auto routing alternates through the pool", func(t *testing.T) {
		models := []config.ModelConfig{
			{Name: "a", Tier: 2, Pool: "qwen", MaxConcurrent: 2},
			{Name: "b", Tier: 2, Pool: "qwen", MaxConcurrent: 2},
			{Name: "fast", Tier: 1},
		}
		heavy := "refactor the parser; debug and investigate the root cause across a.go, b.go, c.go"
		routes, err := routeTasks(models, []string{heavy, heavy, heavy, heavy}, []modelPin{{"auto", 0}}, "models.toml", availFn, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := routes[0].mc.Name + routes[1].mc.Name + routes[2].mc.Name + routes[3].mc.Name
		if got != "abab" {
			t.Errorf("models = %q, want abab", got)
		}
		if !strings.HasPrefix(routes[0].note, "[auto → a") || !strings.HasPrefix(routes[1].note, "[auto → b") {
			t.Errorf("notes = %q, %q; want auto notes naming the member used", routes[0].note, routes[1].note)
		}
	})

	t.Run("models without a pool keep fill-first behavior", func(t *testing.T) {
		models := []config.ModelConfig{
			{Name: "a", Tier: 1, MaxConcurrent: 2},
			{Name: "b", Tier: 1},
		}
		routes, err := routeTasks(models, []string{"t1", "t2", "t3", "t4"}, []modelPin{{"a", 0}}, "models.toml", availFn, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := routes[0].mc.Name + routes[1].mc.Name + routes[2].mc.Name + routes[3].mc.Name
		if got != "aabb" {
			t.Errorf("models = %q, want aabb (fill first, then spill)", got)
		}
	})
}

// TestDelegateVerifyPasses: a passing --verify check keeps the done status
// and appends the verified note to the terse result.
func TestDelegateVerifyPasses(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	_, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--verify", "true", "--task", "build the thing"), &out)
	if err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"w1 done ", "[verified: `true` passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestDelegateVerifyFailsWorker: a failed check on a single-task run flips
// the done worker to failed (per-worker judging), with the check's output
// excerpted into the terse result and the registry.
func TestDelegateVerifyFailsWorker(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--verify", "echo nope-from-check; exit 1",
		"--escalate=false", "--task", "build the thing"), &out)
	if err == nil {
		t.Fatalf("want error, out = %s", out.String())
	}
	got := out.String()
	for _, want := range []string{
		"w1 failed ",
		"[verify FAILED: `echo nope-from-check; exit 1` exit 1",
		"nope-from-check",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].Status != "failed" {
		t.Errorf("workers = %+v", workers)
	}
}

// TestDelegateVerifySkippedWhenUnchanged: the check judges the tree the
// worker left behind — if the worker changed nothing (read-only task),
// running the project's check is pure waste. Fingerprint unchanged means
// the check is skipped entirely: the done status stands and the terse
// result never mentions the check, even when it would have failed.
func TestDelegateVerifySkippedWhenUnchanged(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	stateDir, args := setup(t, f)
	repo := gitRepoDir(t)
	// setup's --dir is args[3]; override with the git repo.
	for i, a := range args {
		if a == "--dir" {
			args[i+1] = repo
		}
	}

	var out strings.Builder
	err := runDelegate(append(args, "--verify", "exit 1",
		"--escalate=false", "--task", "read the code and report"), &out)
	if err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "w1 done ") {
		t.Errorf("output = %q", got)
	}
	if strings.Contains(got, "verify") {
		t.Errorf("output mentions the skipped check: %q", got)
	}
	events, _ := (&registry.Registry{Path: filepath.Join(stateDir, "workers.jsonl")}).Load()
	workers := registry.Reduce(events)
	if len(workers) != 1 || workers[0].Status != "done" {
		t.Errorf("workers = %+v", workers)
	}
}

// TestDelegateVerifyEscalates: a check that fails the first time and passes
// the second drives cheap→strong escalation; the escalated worker is told
// that a previous attempt left the project failing its check.
func TestDelegateVerifyEscalates(t *testing.T) {
	cheap := &fakeOpencode{mode: "done"}
	strong := &fakeOpencode{mode: "done"}
	srvCheap := httptest.NewServer(cheap.handler())
	srvStrong := httptest.NewServer(strong.handler())
	t.Cleanup(srvCheap.Close)
	t.Cleanup(srvStrong.Close)
	stateDir := t.TempDir()
	toml := "[models.cheap]\nendpoint = \"" + srvCheap.URL + "\"\nmodel = \"p/cheap\"\n" +
		"[models.strong]\nendpoint = \"" + srvStrong.URL + "\"\nmodel = \"p/strong\"\n"
	if err := os.WriteFile(filepath.Join(stateDir, "models.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Fails while .count is 0 (first run), passes once it is 1: the
	// counter file lives in --dir, which is where the check runs.
	dir := t.TempDir()
	check := "n=$(cat .count 2>/dev/null || echo 0); echo $((n+1)) > .count; [ \"$n\" -ge 1 ]"
	args := []string{"--state-dir", stateDir, "--dir", dir, "--model", "cheap",
		"--verify", check, "--task", "fix the thing"}

	var out strings.Builder
	if err := runDelegate(args, &out); err != nil {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"escalated from cheap", "[verified:"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	body, _ := strong.lastPrompt.Load().(string)
	if !strings.Contains(body, "failing its check") {
		t.Errorf("escalated prompt missing the verify-failure context: %q", body)
	}
}

// TestDelegateVerifyFromProjectFile: with no --verify flag the project's
// .strawboss.toml supplies the check; --verify off disables it entirely.
func TestDelegateVerifyFromProjectFile(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	_, args := setup(t, f)
	dir := t.TempDir()
	for i, a := range args {
		if a == "--dir" {
			args[i+1] = dir
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".strawboss.toml"), []byte("verify = \"exit 1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := runDelegate(append(args, "--escalate=false", "--task", "build the thing"), &out)
	if err == nil {
		t.Fatalf("want error, out = %s", out.String())
	}
	if !strings.Contains(out.String(), "[verify FAILED: `exit 1`") {
		t.Errorf("output = %q", out.String())
	}

	// --verify off skips the project file's check.
	out.Reset()
	if err := runDelegate(append(args, "--verify", "off", "--escalate=false", "--task", "build it again"), &out); err != nil {
		t.Fatalf("verify off: err = %v, out = %s", err, out.String())
	}
	if strings.Contains(out.String(), "verify") {
		t.Errorf("verify off still mentioned the check: %q", out.String())
	}
}

// TestTimeoutFor: explicit --timeout > model's timeout > default.
func TestTimeoutFor(t *testing.T) {
	tests := []struct {
		name  string
		flag  time.Duration
		model config.ModelConfig
		want  time.Duration
	}{
		{"no flag, unset model value: default", 0, config.ModelConfig{}, defaultWorkerTimeout},
		{"no flag, model value set: model", 0, config.ModelConfig{Timeout: 40 * time.Minute}, 40 * time.Minute},
		{"flag beats model value", 300 * time.Millisecond, config.ModelConfig{Timeout: 40 * time.Minute}, 300 * time.Millisecond},
		{"flag with unset model value", time.Minute, config.ModelConfig{}, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := timeoutFor(tt.flag, tt.model); got != tt.want {
				t.Errorf("timeoutFor(%v, %+v) = %v, want %v", tt.flag, tt.model, got, tt.want)
			}
		})
	}
}

// TestDelegateVerifyBatch: parallel tasks share one directory, so the check
// runs once after all of them; a failure cannot blame a worker, so their
// statuses stand and the call as a whole fails.
func TestDelegateVerifyBatch(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	_, args := setup(t, f)

	var out strings.Builder
	err := runDelegate(append(args, "--verify", "exit 1",
		"--task", "task alpha", "--task", "task beta"), &out)
	if err == nil {
		t.Fatalf("want error, out = %s", out.String())
	}
	got := out.String()
	for _, want := range []string{"w1 done", "w2 done", "after all 2 workers"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestDelegateVerifyBadProjectFile: a broken .strawboss.toml is an error
// before anything spawns — silently skipping the check would pass off
// unverified work as verified.
func TestDelegateVerifyBadProjectFile(t *testing.T) {
	f := &fakeOpencode{mode: "done"}
	_, args := setup(t, f)
	dir := t.TempDir()
	for i, a := range args {
		if a == "--dir" {
			args[i+1] = dir
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".strawboss.toml"), []byte("bogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runDelegate(append(args, "--task", "build the thing"), &out)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, out = %s", err, out.String())
	}
	if n := f.created.Load(); n != 0 {
		t.Errorf("sessions created = %d, want 0", n)
	}
}
