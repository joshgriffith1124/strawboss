// Package harness defines the WorkerHarness boundary between the TUI and
// whatever runs workers. UI code never talks to a worker backend directly —
// only through this interface (CLAUDE.md invariant 4). Implementations:
// opencode (internal/harness/opencode), DeepSeek Harness
// (internal/harness/dshacp), and Claude Code workers for metered frontier
// roles (internal/harness/claudecode).
package harness

import (
	"context"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
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

// Usage is a worker's token consumption so far. InputTokens is FRESH
// input (cache writes included); prefix re-reads live in CacheReadTokens
// so the token economy can compare fresh work to fresh work.
type Usage struct {
	InputTokens     int
	CacheReadTokens int
	OutputTokens    int
	TokensPerSec    float64 // 0 if the backend doesn't report it
	// CostUSD is the notional API cost of a metered worker (the claude
	// harness); 0 for free local workers.
	CostUSD float64
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
	// Report is the worker's whole final reply, uncapped. It never reaches
	// the supervisor: a pipeline hands it to its next step (a plan to the
	// coder, findings back to it) inside the delegate process.
	Report string
}

// readOnlyKey marks a run whose worker must not modify the working tree.
type readOnlyKey struct{}

// WithReadOnly marks ctx's run read-only (a read-only role). The runner
// enforces it for every harness after the fact (edits are reverted and the
// run fails); a harness that can also refuse edits up front — the claude
// harness denies its edit tools — reads it at Spawn.
func WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// ReadOnly reports whether ctx's run is read-only.
func ReadOnly(ctx context.Context) bool {
	ro, _ := ctx.Value(readOnlyKey{}).(bool)
	return ro
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
