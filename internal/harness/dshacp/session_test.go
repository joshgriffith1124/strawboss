package dshacp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture is a real session.jsonl captured from dsh-acp-demo
// 0.1.1-rc.2 (one turn: bash tool call → errored tool result → final
// text; per-step usage 900/40 and 1000/12).
const fixtureSession = "bbe8a47e-16af-4aa1-aafc-b7a0d1ba19e3"

func fixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSessionFixture(t *testing.T) {
	info, err := ReadSession(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if info.Usage.InputTokens != 1900 || info.Usage.OutputTokens != 52 {
		t.Errorf("usage = %+v, want 1900/52", info.Usage)
	}
	if info.LastText != "Created dsh-spike.txt with the expected content." {
		t.Errorf("last text = %q", info.LastText)
	}
	if info.FinishReason != "stop" {
		t.Errorf("finish = %q", info.FinishReason)
	}
	if !info.TurnEnded || info.EndReason != "completed" {
		t.Errorf("turn end = %v %q", info.TurnEnded, info.EndReason)
	}
}

func TestFindSessionLog(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--some-mangled-cwd--", "ses_x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindSessionLog(root, "ses_x")
	if err != nil || !strings.HasSuffix(got, filepath.Join("ses_x", "session.jsonl")) {
		t.Errorf("got %q err %v", got, err)
	}
	if _, err := FindSessionLog(root, "ses_missing"); err == nil {
		t.Error("want error for unknown session")
	}
}

// TestTailSessionIncremental writes the fixture in two halves and checks
// the tailer emits transcript events, cumulative usage, and terminates at
// turn/end.
func TestTailSessionIncremental(t *testing.T) {
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	root := t.TempDir()
	dir := filepath.Join(root, "proj", fixtureSession)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	half := len(lines) / 2
	if err := os.WriteFile(path, []byte(strings.Join(lines[:half], "")), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := TailSession(ctx, root, fixtureSession, 50*time.Millisecond)

	go func() {
		time.Sleep(200 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Join(lines[half:], ""))
		f.Close()
	}()

	var kinds []string
	var lastUsage int
	ended := false
	for it := range ch {
		switch {
		case it.Event != nil:
			kinds = append(kinds, it.Event.Kind)
		case it.Usage != nil:
			lastUsage = it.Usage.OutputTokens
		case it.TurnEnded:
			ended = true
		}
	}
	if !ended {
		t.Error("tailer did not see turn/end")
	}
	if lastUsage != 52 {
		t.Errorf("cumulative output = %d, want 52", lastUsage)
	}
	joined := strings.Join(kinds, ",")
	// tool call, errored tool result, then the final text deltas.
	for _, want := range []string{"tool", "error", "text"} {
		if !strings.Contains(joined, want) {
			t.Errorf("kinds %v missing %q", kinds, want)
		}
	}
}
