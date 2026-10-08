// Package registry tracks workers across processes. Every delegate
// invocation is its own short-lived process (spawned by the supervisor's
// Bash tool), so the registry is an append-only JSONL event log on disk:
// delegate appends queued/spawned/finished events, the TUI replays the file
// to know every worker past and present (queued ones included, waiting for
// a free concurrency slot), and state survives restarts for free.
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
	Type    string    `json:"type"` // "queued" | "spawned" | "finished" | "deadline" | "cost"
	Worker  string    `json:"worker"`
	Session string    `json:"session"`
	// Run scopes workers to one supervisor run so the TUI doesn't replay
	// other runs' history (set from $STRAWBOSS_RUN by delegate).
	Run string `json:"run,omitempty"`
	// queued + spawned fields
	Model string `json:"model,omitempty"`
	Task  string `json:"task,omitempty"`
	Dir   string `json:"dir,omitempty"`
	// PID is the worker's subprocess pid when the harness owns one (dsh);
	// 0 for server-backed workers (opencode). The TUI kills by pid when
	// there is no server to ask.
	PID int `json:"pid,omitempty"`
	// Role is the roles.toml role the worker ran in ("" = plain task),
	// stamped on every event so the TUI and the scorecard can split by it.
	Role string `json:"role,omitempty"`
	// Pipeline and Step place a pipeline step's worker in its chain
	// ("feature", "2/3" or "fix 1 · security"), stamped like Role.
	Pipeline string `json:"pipeline,omitempty"`
	Step     string `json:"step,omitempty"`
	// Deadline is a deadline-event field: the worker's current per-attempt
	// deadline (initial or extended), so the TUI can show time left and
	// operator push-backs.
	Deadline time.Time `json:"deadline,omitempty"`
	// finished fields
	Status          string `json:"status,omitempty"`
	Summary         string `json:"summary,omitempty"`
	LogPath         string `json:"log_path,omitempty"`
	DurationMS      int64  `json:"duration_ms,omitempty"`
	InputTokens     int    `json:"input_tokens,omitempty"`
	CacheReadTokens int    `json:"cache_read_tokens,omitempty"`
	OutputTokens    int    `json:"output_tokens,omitempty"`
	// CostUSD is a cost-event field: a metered worker's notional API cost
	// (a Claude Code worker on the subscription), for the token economy and
	// the budget guard. Local workers never write one.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Registry appends to and replays one workers.jsonl file.
type Registry struct {
	Path string
	// Run is stamped onto every appended event.
	Run string
	// Role is stamped onto every appended event that doesn't carry one.
	// Delegate gives each role task its own copy of the Registry.
	Role string
	// Pipeline and Step are stamped like Role for a pipeline step.
	Pipeline, Step string
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
	if ev.Role == "" {
		ev.Role = r.Role
	}
	if ev.Pipeline == "" {
		ev.Pipeline, ev.Step = r.Pipeline, r.Step
	}
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
// event. Safe across concurrent delegate processes. pid is 0 when the
// harness has no per-worker subprocess.
func (r *Registry) Allocate(session, model, task, dir string, pid int) (string, error) {
	var id string
	err := r.withLock(func() error {
		next, err := r.nextID()
		if err != nil {
			return err
		}
		id = next
		return r.append(Event{
			TS: time.Now(), Type: "spawned", Worker: id, Run: r.Run,
			Session: session, Model: model, Task: task, Dir: dir, PID: pid,
		})
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// nextID returns the next worker id (w1, w2, …) by scanning the event log
// for the highest id — queued workers count, so ids are shared between
// Queue and Allocate. Callers must already hold the lock.
func (r *Registry) nextID() (string, error) {
	events, err := r.Load()
	if err != nil {
		return "", err
	}
	max := 0
	for _, ev := range events {
		n, err := strconv.Atoi(strings.TrimPrefix(ev.Worker, "w"))
		if err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("w%d", max+1), nil
}

// Queue reserves the next worker id and records a queued event: the worker
// may not start yet because its model is at its max_concurrent cap and must
// wait for a free slot. The queued event lets the TUI show the worker
// waiting; when a slot opens, AllocateAs records the spawned event under
// the same id.
func (r *Registry) Queue(model, task, dir string) (string, error) {
	var id string
	err := r.withLock(func() error {
		next, err := r.nextID()
		if err != nil {
			return err
		}
		id = next
		return r.append(Event{
			TS: time.Now(), Type: "queued", Worker: id, Run: r.Run,
			Model: model, Task: task, Dir: dir,
		})
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// AllocateAs records the spawned event for an id that Queue already
// assigned (no new id — the queued worker keeps its place in the sequence).
// pid is 0 when the harness has no per-worker subprocess.
func (r *Registry) AllocateAs(id, session, model, task, dir string, pid int) error {
	return r.withLock(func() error {
		return r.append(Event{
			TS: time.Now(), Type: "spawned", Worker: id, Run: r.Run,
			Session: session, Model: model, Task: task, Dir: dir, PID: pid,
		})
	})
}

// Finish records a worker's terminal state.
func (r *Registry) Finish(workerID, session, status, summary, logPath string, duration time.Duration, inputTokens, cacheReadTokens, outputTokens int) error {
	return r.withLock(func() error {
		return r.append(Event{
			TS: time.Now(), Type: "finished", Worker: workerID, Session: session, Run: r.Run,
			Status: status, Summary: summary, LogPath: logPath,
			DurationMS:  duration.Milliseconds(),
			InputTokens: inputTokens, CacheReadTokens: cacheReadTokens, OutputTokens: outputTokens,
		})
	})
}

// RecordCost records a metered worker's notional cost as a "cost" event
// (after its finished event). Readers that predate it skip the type.
func (r *Registry) RecordCost(worker, model string, usd float64) error {
	return r.withLock(func() error {
		return r.append(Event{TS: time.Now(), Type: "cost", Worker: worker, Run: r.Run, Model: model, CostUSD: usd})
	})
}

// SetDeadline records a worker's current deadline (initial or extended) as a
// "deadline" event.
func (r *Registry) SetDeadline(worker string, at time.Time) error {
	return r.withLock(func() error {
		return r.append(Event{
			TS: time.Now(), Type: "deadline", Worker: worker, Run: r.Run,
			Deadline: at,
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
	ID      string
	Session string
	Model   string
	Task    string
	Dir     string
	// Status is "queued" while the worker waits for a concurrency slot,
	// "running" once spawned, and the finished event's status after that.
	Status   string
	Summary  string
	LogPath  string
	Started  time.Time
	Finished time.Time
	// tokens from the finished event; live counts come from the harness
	InputTokens     int
	CacheReadTokens int
	OutputTokens    int
}

// Reduce folds the event log into per-worker state, in first-event order.
// A "queued" event (the model is at its max_concurrent cap, so the worker
// waits for a free slot) creates the worker with status "queued" and
// Started left zero; a later "spawned" event for that id updates the same
// entry in place instead of adding a second one. A worker with no finished
// event reports as "running" (or "queued" if it never spawned) — the TUI
// can reconcile against the harness for workers orphaned by a crash.
func Reduce(events []Event) []Worker {
	byID := map[string]*Worker{}
	var order []string
	for _, ev := range events {
		switch ev.Type {
		case "queued":
			byID[ev.Worker] = &Worker{
				ID: ev.Worker, Model: ev.Model, Task: ev.Task,
				Dir: ev.Dir, Status: "queued",
			}
			order = append(order, ev.Worker)
		case "spawned":
			if w, ok := byID[ev.Worker]; ok {
				// Spawned after being queued: the slot opened. Keep the
				// same entry instead of adding a duplicate.
				w.Session = ev.Session
				w.Model = ev.Model
				w.Task = ev.Task
				w.Dir = ev.Dir
				w.Status = "running"
				w.Started = ev.TS
				break
			}
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
			w.CacheReadTokens = ev.CacheReadTokens
			w.OutputTokens = ev.OutputTokens
		}
	}
	out := make([]Worker, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
