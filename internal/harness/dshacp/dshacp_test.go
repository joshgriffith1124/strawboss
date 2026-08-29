package dshacp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
)

// fakeBin writes a shell script speaking just enough ACP: initialize and
// session/new replies, then a mode-dependent prompt outcome. mode:
// "done" (update chunk + end_turn, copies the fixture session log),
// "hang" (no prompt reply; cancel settles it), "rpcerr", "empty"
// (end_turn with no chunks and no session log).
func fakeBin(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	copyLog := ""
	if mode == "done" {
		copyLog = fmt.Sprintf("mkdir -p \"$STRAWBOSS_DSH_SESSIONS/proj/%s\"\ncp %q \"$STRAWBOSS_DSH_SESSIONS/proj/%s/session.jsonl\"\n",
			fixtureSession, fixturePath(t), fixtureSession)
	}
	prompt := ""
	switch mode {
	case "done":
		prompt = `echo '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + fixtureSession + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Created dsh-spike.txt with the expected content."}}}}'
      echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'`
	case "hang":
		prompt = `:`
	case "rpcerr":
		prompt = `echo '{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"model route exploded"}}'`
	case "empty":
		prompt = `echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}'`
	}
	script := `#!/bin/sh
` + copyLog + `while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{}}}';;
    *'"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"` + fixtureSession + `"}}';;
    *'"session/prompt"'*) ` + prompt + `;;
    *'"session/cancel"'*) echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}';;
  esac
done
`
	bin := filepath.Join(dir, "dsh-acp-demo")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func testHarness(t *testing.T, mode string) *Harness {
	t.Helper()
	state := t.TempDir()
	cfg := filepath.Join(state, "cordis.yml")
	if err := os.WriteFile(cfg, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(t.TempDir(), state)
	h.Bin = fakeBin(t, mode)
	h.Config = cfg
	h.BootTimeout = 10 * time.Second
	return h
}

func mc() config.ModelConfig {
	return config.ModelConfig{Name: "ds", Endpoint: "http://fake:1/v1", Model: "fake-model", Harness: "dsh"}
}

func TestSpawnResultDone(t *testing.T) {
	h := testHarness(t, "done")
	ctx := context.Background()
	wid, err := h.Spawn(ctx, "do the thing", mc())
	if err != nil {
		t.Fatal(err)
	}
	if wid != fixtureSession {
		t.Errorf("worker id = %q", wid)
	}
	if pid := h.PID(wid); pid <= 0 {
		t.Errorf("pid = %d", pid)
	}
	res, err := h.Result(ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusDone {
		t.Errorf("status = %s (%s)", res.Status, res.Summary)
	}
	if res.Summary != "Created dsh-spike.txt with the expected content." {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.LogPath == "" {
		t.Fatal("no log path")
	}
	if _, err := os.Stat(res.LogPath); err != nil {
		t.Errorf("log missing: %v", err)
	}
	u, err := h.Usage(ctx, wid)
	if err != nil || u.InputTokens != 1900 || u.OutputTokens != 52 {
		t.Errorf("usage = %+v err %v", u, err)
	}
}

func TestStatusTransitions(t *testing.T) {
	h := testHarness(t, "done")
	wid, err := h.Spawn(context.Background(), "task", mc())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Result(context.Background(), wid); err != nil {
		t.Fatal(err)
	}
	st, err := h.Status(context.Background(), wid)
	if err != nil || st != harness.StatusDone {
		t.Errorf("status = %s err %v", st, err)
	}
}

func TestResultAbortedOnCtxCancel(t *testing.T) {
	h := testHarness(t, "hang")
	wid, err := h.Spawn(context.Background(), "task", mc())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := h.Result(ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusFailed || !strings.Contains(res.Summary, "aborted") {
		t.Errorf("res = %+v", res)
	}
}

func TestPromptRPCErrorFails(t *testing.T) {
	h := testHarness(t, "rpcerr")
	wid, err := h.Spawn(context.Background(), "task", mc())
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Result(context.Background(), wid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusFailed || !strings.Contains(res.Summary, "model route exploded") {
		t.Errorf("res = %+v", res)
	}
}

// A clean end_turn with no committed text and no transcript is the
// output-budget signature — must FAIL with advice, never report done
// (docs/NOTES.md retry-loop incident).
func TestEmptyAnswerFailsWithAdvice(t *testing.T) {
	h := testHarness(t, "empty")
	wid, err := h.Spawn(context.Background(), "task", mc())
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Result(context.Background(), wid)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusFailed || !strings.Contains(res.Summary, "Do NOT retry") {
		t.Errorf("res = %+v", res)
	}
}

func TestSpawnMissingBinExplains(t *testing.T) {
	h := testHarness(t, "done")
	h.Bin = filepath.Join(t.TempDir(), "nope")
	if _, err := h.Spawn(context.Background(), "task", mc()); err == nil ||
		!strings.Contains(err.Error(), "dsh-acp-demo bin not found") {
		t.Errorf("err = %v", err)
	}
}

// TestToolsModeReachesSubprocess: models.toml tools_mode flows to the dsh
// subprocess env, where the generated cordis.yml reads it.
func TestToolsModeReachesSubprocess(t *testing.T) {
	h := testHarness(t, "done")
	envDump := filepath.Join(t.TempDir(), "env")
	script := `#!/bin/sh
echo "$STRAWBOSS_DSH_TOOLS_MODE" > ` + envDump + `
while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}';;
    *'"session/new"'*) echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"ses_mode"}}';;
    *'"session/prompt"'*) echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}';;
  esac
done
`
	if err := os.WriteFile(h.Bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := mc()
	m.ToolsMode = "code"
	wid, err := h.Spawn(context.Background(), "task", m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Result(context.Background(), wid); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envDump)
	if err != nil || strings.TrimSpace(string(got)) != "code" {
		t.Errorf("subprocess saw tools mode %q err %v", got, err)
	}
}

// TestParallelSpawnsGetIsolatedSessionRoots: concurrent dsh workers must
// not share a persistence root — the acp app's derived session-query.db
// (SQLite) at that root is single-writer, and sharing it killed 3 of 4
// parallel workers live ("database is locked").
func TestParallelSpawnsGetIsolatedSessionRoots(t *testing.T) {
	dumpDir := t.TempDir()
	script := `#!/bin/sh
echo "$STRAWBOSS_DSH_SESSIONS" > ` + dumpDir + `/root-$$
while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}';;
    *'"session/new"'*) echo "{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"ses-$$\"}}";;
    *'"session/prompt"'*) echo '{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}';;
  esac
done
`
	roots := map[string]bool{}
	for i := 0; i < 2; i++ {
		h := testHarness(t, "empty")
		if err := os.WriteFile(h.Bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		wid, err := h.Spawn(context.Background(), "task", mc())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Result(context.Background(), wid); err != nil {
			t.Fatal(err)
		}
	}
	dumps, err := filepath.Glob(filepath.Join(dumpDir, "root-*"))
	if err != nil || len(dumps) != 2 {
		t.Fatalf("dumps = %v err %v", dumps, err)
	}
	for _, d := range dumps {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Fatal(err)
		}
		roots[strings.TrimSpace(string(b))] = true
	}
	if len(roots) != 2 {
		t.Errorf("workers shared a persistence root: %v", roots)
	}
}
