package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"strawboss/internal/registry"
)

// fakeOpencode serves the minimal API surface delegate exercises. mode
// selects the worker outcome: "done", "failed", or "hang" (never finishes).
type fakeOpencode struct {
	mode    string
	aborts  atomic.Int32
	created atomic.Int32
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
	if w.InputTokens != 2000 || w.OutputTokens != 90 {
		t.Errorf("tokens = %d/%d", w.InputTokens, w.OutputTokens)
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
