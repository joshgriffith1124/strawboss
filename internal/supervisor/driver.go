package supervisor

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxLineBytes bounds a single stream-json line; tool results can be large.
const maxLineBytes = 16 << 20

// Driver runs a Claude Code supervisor as a sequence of headless CLI
// invocations: one `claude -p` subprocess per user message, with context
// carried across turns via --resume <session-id>.
type Driver struct {
	// Command is the claude binary. Empty means "claude".
	Command string
	// PermissionMode is passed as --permission-mode when set. It must cover
	// unattended operation — the supervisor can never block on a prompt.
	PermissionMode string
	// AllowedTools is passed as --allowedTools (comma-joined) when set.
	AllowedTools []string
	// SystemPrompt is passed as --append-system-prompt when set.
	SystemPrompt string
	// Dir is the subprocess working directory. Empty means inherit.
	Dir string
	// Env is appended to the (scrubbed) inherited environment.
	Env []string

	mu        sync.Mutex
	sessionID string
}

// SessionID returns the captured session id ("" before the first init
// event). Later turns resume it automatically.
func (d *Driver) SessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionID
}

// SetSessionID pre-seeds a session to resume (e.g. after a restart).
func (d *Driver) SetSessionID(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionID = id
}

// Turn is one in-flight claude invocation. Read Events until it closes; the
// final event is always a TurnDoneEvent.
type Turn struct {
	Events <-chan Event

	cmd         *exec.Cmd
	interrupted chan struct{}
	done        chan struct{} // closed when the subprocess has exited
	once        sync.Once
}

// PID returns the subprocess pid (0 if not started).
func (t *Turn) PID() int {
	if t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

// Interrupt sends SIGINT (esc-to-interrupt): the CLI ends the current turn
// but the session remains resumable.
func (t *Turn) Interrupt() {
	t.once.Do(func() {
		close(t.interrupted)
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Signal(syscall.SIGINT)
		}
	})
}

// scrubEnv returns env without ANTHROPIC_API_KEY. Subscription auth only
// (invariant 1): an API key in the env would take precedence and start
// billing real dollars.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Start begins a turn with the given user prompt.
func (d *Driver) Start(prompt string) (*Turn, error) {
	bin := d.Command
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", prompt,
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
	if d.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", d.SystemPrompt)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = d.Dir
	cmd.Env = append(scrubEnv(os.Environ()), d.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping claude stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawning %s: %w", bin, err)
	}

	events := make(chan Event, 64)
	t := &Turn{Events: events, cmd: cmd, interrupted: make(chan struct{}), done: make(chan struct{})}

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
		close(t.done)
		interrupted := false
		select {
		case <-t.interrupted:
			interrupted = true
			exitErr = nil // an interrupt is a requested stop, not a failure
		default:
		}
		if exitErr == nil && scanErr != nil {
			exitErr = fmt.Errorf("reading claude stdout: %w", scanErr)
		}
		events <- TurnDoneEvent{ExitErr: exitErr, Stderr: stderr.String(), Interrupted: interrupted}
	}()

	return t, nil
}

// Shutdown terminates an in-flight turn gracefully (SIGTERM); the session
// stays resumable via SessionID. Like Interrupt, a shutdown is a requested
// stop — the TurnDoneEvent reports Interrupted, not an error.
func (t *Turn) Shutdown(grace time.Duration) {
	if t.cmd.Process == nil {
		return
	}
	t.once.Do(func() { close(t.interrupted) })
	_ = t.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-t.done:
	case <-time.After(grace):
		_ = t.cmd.Process.Kill()
	}
}
