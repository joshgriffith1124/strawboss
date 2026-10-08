package claudecode

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

// fakeClaude writes a script that records its args and any API key it
// was handed, then replays a captured stream — no plan quota is spent.
func fakeClaude(t *testing.T, fixture string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	argsFile = filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nenv | grep '^ANTHROPIC_API_KEY=' >> %q || true\ncat %q\n",
		argsFile, argsFile, fixture)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func fixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "supervisor", "testdata", "turn2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRun: a worker runs to a done result on its pinned model with the
// coding tool set, subscription auth only, and its log ends with a done
// record the TUI's tail can follow.
func TestRun(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-must-not-leak")
	bin, argsFile := fakeClaude(t, fixture(t))
	state := t.TempDir()
	h := New(t.TempDir(), state)
	h.Command = bin
	mc := config.ModelConfig{Name: "opus", Harness: "claude", Model: "claude-opus-5-5"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := h.Spawn(ctx, "review the parser", mc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusDone {
		t.Fatalf("status = %s, summary %q", res.Status, res.Summary)
	}
	if res.Report == "" || res.LogPath != LogPath(state, id) {
		t.Errorf("report %q, log %q", res.Report, res.LogPath)
	}
	u, err := h.Usage(ctx, id)
	if err != nil || u.CostUSD <= 0 || u.OutputTokens <= 0 {
		t.Errorf("usage = %+v, %v; want tokens and a notional cost", u, err)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-p", "review the parser", "--model", "claude-opus-5-5", "--permission-mode", "dontAsk",
		strings.Join(DefaultTools, ",")} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	for _, bad := range []string{"--bare", "sk-ant-must-not-leak", "--disallowedTools"} {
		if strings.Contains(string(args), bad) {
			t.Errorf("args contain %q:\n%s", bad, args)
		}
	}

	// The TUI's tail sees transcript or usage records, then the end.
	var sawDone, sawRecord bool
	for it := range Tail(ctx, res.LogPath) {
		if it.Done {
			sawDone = true
		}
		if it.Event != nil || it.Usage != nil {
			sawRecord = true
		}
	}
	if !sawDone || !sawRecord {
		t.Errorf("tail: done %v, records %v", sawDone, sawRecord)
	}
}

// TestReadOnlyDeniesEdits: a read-only run never gets the edit tools.
func TestReadOnlyDeniesEdits(t *testing.T) {
	bin, argsFile := fakeClaude(t, fixture(t))
	h := New(t.TempDir(), t.TempDir())
	h.Command = bin
	ctx, cancel := context.WithTimeout(harness.WithReadOnly(context.Background()), 10*time.Second)
	defer cancel()
	id, err := h.Spawn(ctx, "audit", config.ModelConfig{Name: "opus", Harness: "claude", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Result(ctx, id); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	lines := strings.Split(string(args), "\n")
	for i, l := range lines {
		switch l {
		case "--allowedTools":
			if strings.Contains(lines[i+1], "Edit") || strings.Contains(lines[i+1], "Write") {
				t.Errorf("read-only allow list has edit tools: %s", lines[i+1])
			}
		case "--disallowedTools":
			if lines[i+1] != "Edit,Write,NotebookEdit" {
				t.Errorf("deny list = %q", lines[i+1])
			}
		}
	}
	if !strings.Contains(string(args), "--disallowedTools") {
		t.Errorf("read-only run passed no --disallowedTools:\n%s", args)
	}
}

func TestTools(t *testing.T) {
	custom := config.ModelConfig{AllowedTools: []string{"Read", "Edit", "Bash(go test:*)"}}
	allowed, denied := Tools(custom, false)
	if strings.Join(allowed, ",") != "Read,Edit,Bash(go test:*)" || denied != nil {
		t.Errorf("custom = %v / %v", allowed, denied)
	}
	allowed, denied = Tools(custom, true)
	if strings.Join(allowed, ",") != "Read,Bash(go test:*)" || len(denied) == 0 {
		t.Errorf("custom read-only = %v / %v", allowed, denied)
	}
}

// TestExitWithoutResult: a claude process that dies before a result line
// fails the worker with the reason.
func TestExitWithoutResult(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'not logged in' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := New(t.TempDir(), t.TempDir())
	h.Command = bin
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := h.Spawn(ctx, "x", config.ModelConfig{Name: "opus", Harness: "claude", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != harness.StatusFailed || !strings.Contains(res.Summary, "not logged in") {
		t.Errorf("result = %+v", res)
	}
}
