package main

import (
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

	"github.com/joshgriffith1124/strawboss/internal/live"
	"github.com/joshgriffith1124/strawboss/internal/registry"
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
		{"missing model", []string{"--task", "x"}, "at least one --task"},
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
