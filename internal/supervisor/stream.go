package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Stream is a persistent supervisor process: one `claude -p` with
// --input-format stream-json, alive across turns. User messages are
// injected over stdin at any time — INCLUDING mid-turn, which the CLI
// delivers into the running turn (see docs/NOTES.md).
// The process loads its context once instead of once per turn.
type Stream struct {
	Events <-chan Event

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writeMu  sync.Mutex
	done     chan struct{}
	stopping atomic.Bool
}

// StartStream spawns the persistent supervisor. The stream ends (Events
// closes after a final TurnDoneEvent) only when the process exits —
// crash, Interrupt, or Shutdown; the session resumes via the driver's
// captured session id.
func (d *Driver) StartStream() (*Stream, error) {
	bin := d.Command
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
	}
	if sid := d.SessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if d.PermissionMode != "" {
		args = append(args, "--permission-mode", d.PermissionMode)
	}
	if len(d.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(d.AllowedTools, ","))
	}
	if len(d.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(d.DisallowedTools, ","))
	}
	if d.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", d.SystemPrompt)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = d.Dir
	cmd.Env = append(scrubEnv(os.Environ()), d.env()...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("piping claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping claude stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawning %s: %w", bin, err)
	}

	events := make(chan Event, 64)
	s := &Stream{Events: events, cmd: cmd, stdin: stdin, done: make(chan struct{})}

	go func() {
		defer close(events)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxLineBytes)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			ev, _ := ParseLine(line)
			if ev == nil {
				continue
			}
			if init, ok := ev.(InitEvent); ok && init.SessionID != "" {
				d.SetSessionID(init.SessionID)
			}
			events <- ev
		}
		scanErr := sc.Err()
		exitErr := cmd.Wait()
		close(s.done)
		interrupted := s.stopping.Load()
		if interrupted {
			exitErr = nil // requested stop, not a failure
		}
		if exitErr == nil && scanErr != nil {
			exitErr = fmt.Errorf("reading claude stdout: %w", scanErr)
		}
		events <- TurnDoneEvent{ExitErr: exitErr, Stderr: stderr.String(), Interrupted: interrupted}
	}()

	return s, nil
}

// PID returns the subprocess pid (0 if not started).
func (s *Stream) PID() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Alive reports whether the process is still running.
func (s *Stream) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Send injects a user message. Works any time, including while a turn is
// running (the CLI delivers it into the turn).
func (s *Stream) Send(text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding user message: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.Alive() {
		return fmt.Errorf("supervisor process is gone")
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("sending to supervisor: %w", err)
	}
	return nil
}

// Interrupt sends SIGINT. In stream mode this ends the whole process
// — the caller respawns with --resume,
// which is exactly the recovery path a crash takes.
func (s *Stream) Interrupt() {
	s.stopping.Store(true)
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGINT)
	}
}

// Shutdown ends the process gracefully: stdin closes (the CLI exits when
// input ends), then SIGTERM, then kill. The session stays resumable.
func (s *Stream) Shutdown(grace time.Duration) {
	s.stopping.Store(true)
	s.writeMu.Lock()
	_ = s.stdin.Close()
	s.writeMu.Unlock()
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	}
}
