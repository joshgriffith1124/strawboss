package supervisor

import (
	"encoding/json"
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
  case "$line" in
  *control_request*)
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"interrupt-1","response":{"still_queued":[]}}}'
    echo '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"usage":{"input_tokens":1,"output_tokens":0}}'
    continue;;
  esac
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
	// The interrupt is a control message, not a signal: the turn closes
	// with an interrupted (not failed) result and the process lives on.
	s.Interrupt()
	events := collectStream(t, s, func(ev Event) bool { _, ok := ev.(ResultEvent); return ok })
	if _, ok := events[len(events)-2].(ControlResponseEvent); !ok {
		t.Errorf("no control response before the result: %#v", events)
	}
	res := events[len(events)-1].(ResultEvent)
	if !res.Interrupted || !res.IsError || res.Subtype != "error_during_execution" {
		t.Errorf("result = %+v", res)
	}
	if !s.Alive() {
		t.Fatal("process died on interrupt")
	}
	// The next prompt's result is a normal one — the interrupt flag does
	// not leak forward.
	if err := s.Send("again"); err != nil {
		t.Fatal(err)
	}
	events = collectStream(t, s, func(ev Event) bool { _, ok := ev.(ResultEvent); return ok })
	if res := events[len(events)-1].(ResultEvent); res.Interrupted || res.IsError {
		t.Errorf("post-interrupt result = %+v", res)
	}
	s.Shutdown(2 * time.Second)
	events = collectStream(t, s, func(ev Event) bool { _, ok := ev.(TurnDoneEvent); return ok })
	if done := events[len(events)-1].(TurnDoneEvent); done.ExitErr != nil {
		t.Errorf("shutdown = %+v", done)
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

// TestUserMessageImageThenText: SendContent writes ONE json line whose
// content array is image blocks first, then the text block.
func TestUserMessageImageThenText(t *testing.T) {
	b, err := userMessage("look", []Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n") {
		t.Fatalf("userMessage should be a single line: %q", b)
	}
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, b)
	}
	if msg["type"] != "user" {
		t.Fatalf("type = %v", msg["type"])
	}
	content, ok := msg["message"].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content not []any: %#v", msg)
	}
	if len(content) != 2 {
		t.Fatalf("content blocks = %#v", content)
	}
	img := content[0].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("block[0] = %#v", img)
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AQID" {
		t.Fatalf("source = %#v", src)
	}
	txt := content[1].(map[string]any)
	if txt["type"] != "text" || txt["text"] != "look" {
		t.Fatalf("block[1] = %#v", txt)
	}
}

// TestUserMessageImagesOnly: empty text still sends the image blocks,
// and the text block is omitted entirely.
func TestUserMessageImagesOnly(t *testing.T) {
	b, err := userMessage("", []Image{{MediaType: "image/png", Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, b)
	}
	content := msg["message"].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks = %#v", content)
	}
	if content[0].(map[string]any)["type"] != "image" {
		t.Fatalf("block[0] = %#v", content[0])
	}
}

// TestUserMessageEmptyRejects: no text and no images is a wire error, not
// an empty message.
func TestUserMessageEmptyRejects(t *testing.T) {
	if _, err := userMessage("", nil); err == nil {
		t.Fatal("expected error for empty message")
	}
	if _, err := userMessage("   ", nil); err == nil {
		t.Fatal("expected error for whitespace-only text with no images")
	}
	// Whitespace-only text with images still sends the image block, no text block.
	b, err := userMessage("   ", []Image{{MediaType: "image/png", Data: []byte{9}}})
	if err != nil {
		t.Fatalf("images with whitespace text: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["message"].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "image" {
		t.Fatalf("content = %#v", content)
	}
}
