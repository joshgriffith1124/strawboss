// Package harness defines the WorkerHarness boundary between the TUI and
// whatever runs workers. UI code never talks to a worker backend directly —
// only through this interface (CLAUDE.md invariant 4). The only v1
// implementation is opencode (internal/harness/opencode, built in M2); no
// second implementation until one is actually wanted.
package harness

import (
	"context"
	"time"

	"strawboss/internal/config"
)

// Status is a worker's lifecycle state.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Event is one item in a worker's live transcript (tool call, output chunk,
// diff) for the detail pane. Kind and Text are deliberately loose: the TUI
// renders them, it does not interpret them.
type Event struct {
	Time time.Time
	Kind string // e.g. "tool", "text", "error"
	Text string
}

// Usage is a worker's token consumption so far.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TokensPerSec float64 // 0 if the backend doesn't report it
}

// Result is what the delegate command hands back to the supervisor: the
// terse-result contract (CLAUDE.md invariant 3). Summary must stay a few
// lines; the full transcript lives at LogPath and flows to the TUI through
// Events, never through the supervisor.
type Result struct {
	WorkerID string
	Status   Status
	Summary  string
	LogPath  string
}

// WorkerHarness is exactly what the TUI and the delegate command need from a
// worker backend, and nothing more.
type WorkerHarness interface {
	// Spawn starts a task on the given model config and returns a worker id.
	Spawn(ctx context.Context, task string, model config.ModelConfig) (workerID string, err error)
	// Status reports the worker's current lifecycle state.
	Status(ctx context.Context, workerID string) (Status, error)
	// Events streams the worker's live transcript. The channel closes when
	// the worker finishes or the context is cancelled.
	Events(ctx context.Context, workerID string) (<-chan Event, error)
	// Usage reports token counts so far.
	Usage(ctx context.Context, workerID string) (Usage, error)
	// Result blocks until the worker finishes and returns the terse result.
	Result(ctx context.Context, workerID string) (Result, error)
}
