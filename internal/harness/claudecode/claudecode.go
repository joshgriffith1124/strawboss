// Package claudecode runs Claude Code workers: one headless `claude -p`
// per task, on the model its config pins, in the worker's directory. It is
// the third WorkerHarness (after opencode and dsh), for roles that want a
// frontier model — architecture, security review. Subscription auth only,
// exactly like the supervisor (invariant 1): the spawn goes through the
// supervisor package's driver, which scrubs ANTHROPIC_API_KEY and never
// passes --bare. Runs are metered (config.ModelConfig.Metered): routing
// never picks one on its own.
package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
)

// DefaultTools is the worker's --allowedTools when its model config sets
// none: enough to read, edit, and run the project's tests.
var DefaultTools = []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash"}

// editTools are denied to a read-only role's worker up front (the runner's
// snapshot check still backstops edits made through Bash).
var editTools = []string{"Edit", "Write", "NotebookEdit"}

// maxSummaryBytes caps the terse summary (CLAUDE.md invariant 3).
const maxSummaryBytes = 700

// Harness spawns and tracks Claude Code workers for one directory.
type Harness struct {
	Dir      string
	StateDir string
	// Command is the claude binary; "" means "claude".
	Command string

	mu      sync.Mutex
	workers map[string]*worker
}

// New returns a harness running workers in dir, logging under stateDir.
func New(dir, stateDir string) *Harness {
	return &Harness{Dir: dir, StateDir: stateDir, workers: map[string]*worker{}}
}

type worker struct {
	turn    *supervisor.Turn
	logPath string
	done    chan struct{}

	mu       sync.Mutex
	usage    map[string]supervisor.Usage // per API message: one response spans several lines
	result   *supervisor.ResultEvent
	lastText string
	exitErr  error
	stderr   string
}

// newID mints a worker id; the registry records it as the session, and the
// TUI derives the log path from it.
func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "cc-" + hex.EncodeToString(b[:])
}

// Tools is the --allowedTools / --disallowedTools pair for a worker: the
// config's list (or DefaultTools), and for a read-only run the edit tools
// removed from the allow list and denied outright.
func Tools(mc config.ModelConfig, readOnly bool) (allowed, denied []string) {
	allowed = mc.AllowedTools
	if len(allowed) == 0 {
		allowed = DefaultTools
	}
	if !readOnly {
		return append([]string(nil), allowed...), nil
	}
	edit := map[string]bool{}
	for _, t := range editTools {
		edit[t] = true
	}
	var keep []string
	for _, t := range allowed {
		if !edit[t] {
			keep = append(keep, t)
		}
	}
	return keep, append([]string(nil), editTools...)
}

// Spawn starts `claude -p` on the task with the config's model. The
// worker runs non-interactively (dontAsk): a tool outside its allow list
// is refused, never prompted for.
func (h *Harness) Spawn(ctx context.Context, task string, mc config.ModelConfig) (string, error) {
	id := newID()
	logPath := LogPath(h.StateDir, id)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", fmt.Errorf("claude worker log dir: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("claude worker log: %w", err)
	}
	allowed, denied := Tools(mc, harness.ReadOnly(ctx))
	d := &supervisor.Driver{
		Command:         h.Command,
		Model:           mc.Model,
		PermissionMode:  "dontAsk",
		AllowedTools:    allowed,
		DisallowedTools: denied,
		Dir:             h.Dir,
	}
	turn, err := d.Start(task)
	if err != nil {
		f.Close()
		return "", fmt.Errorf("claude worker: %w", err)
	}
	w := &worker{turn: turn, logPath: logPath, done: make(chan struct{}), usage: map[string]supervisor.Usage{}}
	h.mu.Lock()
	if h.workers == nil {
		h.workers = map[string]*worker{}
	}
	h.workers[id] = w
	h.mu.Unlock()
	go w.pump(f)
	return id, nil
}

// pump turns the claude stream into worker-log records and the worker's
// final state.
func (w *worker) pump(f *os.File) {
	defer close(w.done)
	defer f.Close()
	enc := json.NewEncoder(f)
	write := func(r record) {
		r.TS = time.Now()
		_ = enc.Encode(r)
	}
	for ev := range w.turn.Events {
		switch e := ev.(type) {
		case supervisor.AssistantEvent:
			if t := strings.TrimSpace(e.Thinking); t != "" {
				write(record{Kind: "reasoning", Text: clip(t, 400)})
			}
			if t := strings.TrimSpace(e.Text); t != "" {
				w.mu.Lock()
				w.lastText = t
				w.mu.Unlock()
				write(record{Kind: "text", Text: clip(t, 2000)})
			}
			for _, tu := range e.ToolUses {
				write(record{Kind: "tool", Text: toolLine(tu)})
			}
			if e.MessageID != "" {
				w.mu.Lock()
				w.usage[e.MessageID] = e.Usage
				u := w.sumLocked()
				w.mu.Unlock()
				write(record{Usage: &usageRec{In: u.InputTokens, CacheRead: u.CacheReadTokens, Out: u.OutputTokens}, Ctx: e.Usage.Total()})
			}
		case supervisor.ToolResultsEvent:
			for _, r := range e.Results {
				if r.IsError {
					write(record{Kind: "error", Text: clip(firstLine(r.Content), 200)})
				}
			}
		case supervisor.ResultEvent:
			w.mu.Lock()
			w.result = &e
			w.mu.Unlock()
		case supervisor.TurnDoneEvent:
			w.mu.Lock()
			w.exitErr, w.stderr = e.ExitErr, e.Stderr
			w.mu.Unlock()
		}
	}
	status, _ := w.outcome()
	w.mu.Lock()
	cost := 0.0
	if w.result != nil {
		cost = w.result.TotalCostUSD
	}
	w.mu.Unlock()
	write(record{Done: true, Status: string(status), Cost: cost})
}

// sumLocked totals per-message usage; w.mu must be held.
func (w *worker) sumLocked() harness.Usage {
	var u harness.Usage
	for _, m := range w.usage {
		u.InputTokens += m.InputTokens + m.CacheCreationTokens
		u.CacheReadTokens += m.CacheReadTokens
		u.OutputTokens += m.OutputTokens
	}
	return u
}

// outcome is the worker's final status and summary text (uncapped).
func (w *worker) outcome() (harness.Status, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.result != nil && !w.result.IsError && w.result.Subtype == "success":
		text := strings.TrimSpace(w.result.Result)
		if text == "" {
			text = w.lastText
		}
		if text == "" {
			text = "(empty reply)"
		}
		return harness.StatusDone, text
	case w.result != nil:
		return harness.StatusFailed, strings.TrimSpace(fmt.Sprintf("claude worker ended with %s. %s", w.result.Subtype, w.lastText))
	case w.exitErr != nil:
		msg := "claude worker exited: " + w.exitErr.Error()
		if s := strings.TrimSpace(w.stderr); s != "" {
			msg += ": " + clip(s, 300)
		}
		return harness.StatusFailed, msg
	}
	return harness.StatusFailed, "claude worker stopped without a result"
}

func (h *Harness) worker(id string) (*worker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.workers[id]
	if w == nil {
		return nil, fmt.Errorf("worker %s: not spawned by this process", id)
	}
	return w, nil
}

// Status is running until the claude process has finished.
func (h *Harness) Status(ctx context.Context, workerID string) (harness.Status, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return "", err
	}
	select {
	case <-w.done:
		status, _ := w.outcome()
		return status, nil
	default:
		return harness.StatusRunning, nil
	}
}

// Events streams the worker's transcript from its log.
func (h *Harness) Events(ctx context.Context, workerID string) (<-chan harness.Event, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return nil, err
	}
	out := make(chan harness.Event, 64)
	go func() {
		defer close(out)
		for it := range Tail(ctx, w.logPath) {
			if it.Event == nil {
				continue
			}
			select {
			case out <- *it.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Usage is the worker's tokens so far — the result's totals once it has
// finished — and its notional cost.
func (h *Harness) Usage(ctx context.Context, workerID string) (harness.Usage, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return harness.Usage{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.result; r != nil {
		return harness.Usage{
			InputTokens:     r.Usage.InputTokens + r.Usage.CacheCreationTokens,
			CacheReadTokens: r.Usage.CacheReadTokens,
			OutputTokens:    r.Usage.OutputTokens,
			CostUSD:         r.TotalCostUSD,
		}, nil
	}
	return w.sumLocked(), nil
}

// Result blocks until the worker finishes (or ctx ends — the runner then
// aborts it) and returns the terse result plus the full report.
func (h *Harness) Result(ctx context.Context, workerID string) (harness.Result, error) {
	w, err := h.worker(workerID)
	if err != nil {
		return harness.Result{}, err
	}
	select {
	case <-w.done:
	case <-ctx.Done():
		return harness.Result{}, ctx.Err()
	}
	status, text := w.outcome()
	summary := text
	if len(summary) > maxSummaryBytes {
		summary = summary[:maxSummaryBytes] + "…"
	}
	report := ""
	if status == harness.StatusDone {
		report = text
	}
	return harness.Result{WorkerID: workerID, Status: status, Summary: summary, LogPath: w.logPath, Report: report}, nil
}

// Abort stops the worker's claude process (SIGTERM, then kill).
func (h *Harness) Abort(ctx context.Context, workerID string) error {
	w, err := h.worker(workerID)
	if err != nil {
		return err
	}
	w.turn.Shutdown(5 * time.Second)
	return nil
}

// PID is the worker's claude process, for the TUI's kill.
func (h *Harness) PID(workerID string) int {
	w, err := h.worker(workerID)
	if err != nil {
		return 0
	}
	return w.turn.PID()
}

// toolLine renders a tool call for the transcript: the name and its most
// telling argument.
func toolLine(tu supervisor.ToolUse) string {
	if cmd, ok := tu.BashCommand(); ok {
		return "Bash " + clip(firstLine(cmd), 160)
	}
	var in struct {
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(tu.Input, &in)
	for _, arg := range []string{in.FilePath, in.Pattern, in.Path} {
		if arg != "" {
			return tu.Name + " " + clip(arg, 160)
		}
	}
	return tu.Name
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
