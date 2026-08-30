package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScrubEnv(t *testing.T) {
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-ant-secret", "HOME=/home/x"}
	got := scrubEnv(env)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, kv := range got {
		if strings.Contains(kv, "ANTHROPIC_API_KEY") {
			t.Fatalf("key leaked: %v", got)
		}
	}
}

// fakeClaude writes a script that records its args and env, then replays a
// captured fixture stream — the driver is tested without touching the plan.
func fakeClaude(t *testing.T, fixture string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	argsFile = filepath.Join(dir, "args")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
env | grep '^ANTHROPIC_API_KEY=' >> %q || true
cat %q
`, argsFile, argsFile, fixture)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func collect(t *testing.T, tr *Turn) []Event {
	t.Helper()
	var events []Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-tr.Events:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timed out waiting for events")
		}
	}
}

func TestDriverTurnAndResume(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "turn1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bin, argsFile := fakeClaude(t, fixture)

	d := &Driver{
		Command:        bin,
		PermissionMode: "dontAsk",
		AllowedTools:   []string{"Bash(strawboss delegate:*)", "Read"},
	}

	// First turn: no --resume, session id captured from init.
	tr, err := d.Start("hello")
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, tr)

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-p", "hello", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages", "--permission-mode", "dontAsk",
		"Bash(strawboss delegate:*),Read"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "--resume") {
		t.Errorf("first turn must not resume:\n%s", args)
	}
	if strings.Contains(string(args), "ANTHROPIC_API_KEY") {
		t.Errorf("API key leaked into subprocess env:\n%s", args)
	}

	if d.SessionID() != wantSession {
		t.Errorf("session = %q", d.SessionID())
	}
	last, ok := events[len(events)-1].(TurnDoneEvent)
	if !ok {
		t.Fatalf("last event = %T, want TurnDoneEvent", events[len(events)-1])
	}
	if last.ExitErr != nil || last.Interrupted {
		t.Errorf("turn done = %+v", last)
	}
	var sawResult bool
	for _, ev := range events {
		if _, ok := ev.(ResultEvent); ok {
			sawResult = true
		}
	}
	if !sawResult {
		t.Error("no ResultEvent in stream")
	}

	// Second turn resumes the captured session.
	tr2, err := d.Start("again")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, tr2)
	args2, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args2), "--resume\n"+wantSession) {
		t.Errorf("second turn missing --resume %s:\n%s", wantSession, args2)
	}
}

func TestDriverExitError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Driver{Command: bin}
	tr, err := d.Start("x")
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, tr)
	last, ok := events[len(events)-1].(TurnDoneEvent)
	if !ok {
		t.Fatalf("last event = %T", events[len(events)-1])
	}
	if last.ExitErr == nil {
		t.Error("want exit error")
	}
	if !strings.Contains(last.Stderr, "boom") {
		t.Errorf("stderr = %q", last.Stderr)
	}
}

func TestDriverInterrupt(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// Emits init then sleeps; SIGINT should end the turn cleanly.
	script := `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s1","apiKeySource":"none"}'
exec sleep 30
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Driver{Command: bin}
	tr, err := d.Start("x")
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the init event so the process is definitely up.
	select {
	case <-tr.Events:
	case <-time.After(10 * time.Second):
		t.Fatal("no init event")
	}
	tr.Interrupt()
	events := collect(t, tr)
	last, ok := events[len(events)-1].(TurnDoneEvent)
	if !ok {
		t.Fatalf("last event = %T", events[len(events)-1])
	}
	if !last.Interrupted {
		t.Error("want Interrupted=true")
	}
	if last.ExitErr != nil {
		t.Errorf("interrupt is not a failure: %v", last.ExitErr)
	}
}

func TestSetEnvVarReplacesAndAppends(t *testing.T) {
	d := &Driver{Env: []string{"STRAWBOSS_RUN=run-old", "OTHER=x"}}
	d.SetEnvVar("STRAWBOSS_RUN", "run-new")
	if got := d.EnvVar("STRAWBOSS_RUN"); got != "run-new" {
		t.Errorf("STRAWBOSS_RUN = %q", got)
	}
	if got := d.EnvVar("OTHER"); got != "x" {
		t.Errorf("OTHER = %q", got)
	}
	d.SetEnvVar("ADDED", "y")
	if got := d.EnvVar("ADDED"); got != "y" {
		t.Errorf("ADDED = %q", got)
	}
	if n := len(d.env()); n != 3 {
		t.Errorf("env entries = %d, want 3 (replace must not append)", n)
	}
}
