package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeStreamClaude echoes an init line, then answers every stdin line with
// an assistant + result pair — a stand-in for the persistent CLI.
func fakeStreamClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"stream-s1","apiKeySource":"none"}'
n=0
while read line; do
  n=$((n+1))
  echo '{"type":"assistant","message":{"id":"m'$n'","content":[{"type":"text","text":"reply '$n'"}],"usage":{"input_tokens":1,"output_tokens":2}}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"reply '$n'","total_cost_usd":0.01,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":2}}'
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func collectStream(t *testing.T, s *Stream, want func(Event) bool) []Event {
	t.Helper()
	var events []Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events:
			if !ok {
				t.Fatalf("events closed early; got %#v", events)
			}
			events = append(events, ev)
			if want(ev) {
				return events
			}
		case <-timeout:
			t.Fatalf("timed out; got %#v", events)
		}
	}
}

func TestStreamMultiTurnOneProcess(t *testing.T) {
	d := &Driver{Command: fakeStreamClaude(t)}
	s, err := d.StartStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(2 * time.Second)

	if err := s.Send("first"); err != nil {
		t.Fatal(err)
	}
	events := collectStream(t, s, func(ev Event) bool { _, ok := ev.(ResultEvent); return ok })
	if d.SessionID() != "stream-s1" {
		t.Errorf("session = %q", d.SessionID())
	}
	pid := s.PID()
	if pid == 0 {
		t.Error("pid = 0")
	}

	// Second turn on the SAME process.
	if err := s.Send("second"); err != nil {
		t.Fatal(err)
	}
	events = collectStream(t, s, func(ev Event) bool { _, ok := ev.(ResultEvent); return ok })
	found := false
	for _, ev := range events {
		if a, ok := ev.(AssistantEvent); ok && strings.Contains(a.Text, "reply 2") {
			found = true
		}
	}
	if !found {
		t.Errorf("second reply missing from %#v", events)
	}
	if s.PID() != pid {
		t.Errorf("process changed: %d → %d", pid, s.PID())
	}
	if !s.Alive() {
		t.Error("stream not alive after two turns")
	}
}

func TestStreamInterrupt(t *testing.T) {
	d := &Driver{Command: fakeStreamClaude(t)}
	s, err := d.StartStream()
	if err != nil {
		t.Fatal(err)
	}
	// Wait for init so the process is definitely up.
	collectStream(t, s, func(ev Event) bool { _, ok := ev.(InitEvent); return ok })
	s.Interrupt()
	events := collectStream(t, s, func(ev Event) bool { _, ok := ev.(TurnDoneEvent); return ok })
	done := events[len(events)-1].(TurnDoneEvent)
	if !done.Interrupted || done.ExitErr != nil {
		t.Errorf("done = %+v", done)
	}
	if s.Alive() {
		t.Error("still alive after interrupt")
	}
	if err := s.Send("nope"); err == nil {
		t.Error("Send to a dead stream must error")
	}
}

func TestStreamShutdownGraceful(t *testing.T) {
	d := &Driver{Command: fakeStreamClaude(t)}
	s, err := d.StartStream()
	if err != nil {
		t.Fatal(err)
	}
	collectStream(t, s, func(ev Event) bool { _, ok := ev.(InitEvent); return ok })
	start := time.Now()
	s.Shutdown(5 * time.Second)
	// stdin close ends the read loop — well before the grace timeout.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("shutdown took %v; stdin close should have ended it", elapsed)
	}
	events := collectStream(t, s, func(ev Event) bool { _, ok := ev.(TurnDoneEvent); return ok })
	done := events[len(events)-1].(TurnDoneEvent)
	if !done.Interrupted || done.ExitErr != nil {
		t.Errorf("done = %+v", done)
	}
}

func TestStreamResumeArgs(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	argsFile := filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nread line\n", argsFile)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Driver{Command: bin, PermissionMode: "dontAsk", AllowedTools: []string{"Read"}}
	d.SetSessionID("resume-me")
	s, err := d.StartStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(time.Second)
	time.Sleep(300 * time.Millisecond)
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--input-format", "stream-json", "--resume", "resume-me", "dontAsk"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
}
