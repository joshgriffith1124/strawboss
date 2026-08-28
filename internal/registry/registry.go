// Package registry tracks workers across processes. Every delegate
// invocation is its own short-lived process (spawned by the supervisor's
// Bash tool), so the registry is an append-only JSONL event log on disk:
// delegate appends spawned/finished events, the TUI replays the file to
// know every worker past and present, and state survives restarts for free.
package registry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Event is one line of workers.jsonl.
type Event struct {
	TS      time.Time `json:"ts"`
	Type    string    `json:"type"` // "spawned" | "finished"
	Worker  string    `json:"worker"`
	Session string    `json:"session"`
	// Run scopes workers to one supervisor run so the TUI doesn't replay
	// other runs' history (set from $STRAWBOSS_RUN by delegate).
	Run string `json:"run,omitempty"`
	// spawned fields
	Model string `json:"model,omitempty"`
	Task  string `json:"task,omitempty"`
	Dir   string `json:"dir,omitempty"`
	// finished fields
	Status       string `json:"status,omitempty"`
	Summary      string `json:"summary,omitempty"`
	LogPath      string `json:"log_path,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
}

// Registry appends to and replays one workers.jsonl file.
type Registry struct {
	Path string
	// Run is stamped onto every appended event.
	Run string
}

// withLock serializes multi-process access (concurrent delegations) around
// a sidecar flock file.
func (r *Registry) withLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return fmt.Errorf("registry dir: %w", err)
	}
	lock, err := os.OpenFile(r.Path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("registry lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("registry lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (r *Registry) append(ev Event) error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("registry append: %w", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(ev); err != nil {
		return fmt.Errorf("registry append: %w", err)
	}
	return nil
}

// Allocate assigns the next worker id (w1, w2, …) and records the spawned
// event. Safe across concurrent delegate processes.
func (r *Registry) Allocate(session, model, task, dir string) (string, error) {
	var id string
	err := r.withLock(func() error {
		events, err := r.Load()
		if err != nil {
			return err
		}
		max := 0
		for _, ev := range events {
			n, err := strconv.Atoi(strings.TrimPrefix(ev.Worker, "w"))
			if err == nil && n > max {
				max = n
			}
		}
		id = fmt.Sprintf("w%d", max+1)
		return r.append(Event{
			TS: time.Now(), Type: "spawned", Worker: id, Run: r.Run,
			Session: session, Model: model, Task: task, Dir: dir,
		})
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Finish records a worker's terminal state.
func (r *Registry) Finish(workerID, session, status, summary, logPath string, duration time.Duration, inputTokens, outputTokens int) error {
	return r.withLock(func() error {
		return r.append(Event{
			TS: time.Now(), Type: "finished", Worker: workerID, Session: session, Run: r.Run,
			Status: status, Summary: summary, LogPath: logPath,
			DurationMS:  duration.Milliseconds(),
			InputTokens: inputTokens, OutputTokens: outputTokens,
		})
	})
}

// Load replays the event log. A missing file is an empty history; a
// corrupt line is skipped, never fatal.
func (r *Registry) Load() ([]Event, error) {
	f, err := os.Open(r.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry load: %w", err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("registry load: %w", err)
	}
	return events, nil
}

// Worker is the reduced current state of one worker.
type Worker struct {
	ID       string
	Session  string
	Model    string
	Task     string
	Dir      string
	Status   string // "running" until a finished event lands
	Summary  string
	LogPath  string
	Started  time.Time
	Finished time.Time
	// tokens from the finished event; live counts come from the harness
	InputTokens  int
	OutputTokens int
}

// Reduce folds the event log into per-worker state, in spawn order. A
// spawned worker with no finished event reports as "running" — the TUI can
// reconcile against the harness for workers orphaned by a crash.
func Reduce(events []Event) []Worker {
	byID := map[string]*Worker{}
	var order []string
	for _, ev := range events {
		switch ev.Type {
		case "spawned":
			byID[ev.Worker] = &Worker{
				ID: ev.Worker, Session: ev.Session, Model: ev.Model,
				Task: ev.Task, Dir: ev.Dir, Status: "running", Started: ev.TS,
			}
			order = append(order, ev.Worker)
		case "finished":
			w, ok := byID[ev.Worker]
			if !ok {
				continue
			}
			w.Status = ev.Status
			w.Summary = ev.Summary
			w.LogPath = ev.LogPath
			w.Finished = ev.TS
			w.InputTokens = ev.InputTokens
			w.OutputTokens = ev.OutputTokens
		}
	}
	out := make([]Worker, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
