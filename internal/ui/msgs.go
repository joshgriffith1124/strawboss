package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The feed vocabulary. Every external source (supervisor driver, harness
// poller, replay) emits these; the UI knows nothing about where they come
// from.

// SupInitMsg announces the supervisor subprocess/session.
type SupInitMsg struct {
	SessionID string
	Model     string
	Auth      string // "subscription" or a warning
	PID       int
}

// SupUserMsg echoes a user prompt into the chat (replay uses it; live mode
// adds the user's own input directly).
type SupUserMsg struct {
	Text string
	Time time.Time
}

// SupTextDeltaMsg is streamed assistant text.
type SupTextDeltaMsg struct{ Text string }

// SupTextDoneMsg finalizes the in-progress assistant message.
type SupTextDoneMsg struct {
	Text string // authoritative full text (replaces accumulated deltas)
	Time time.Time
}

// SupToolMsg is a supervisor tool call shown inline in chat. For
// delegations, Delegate carries the parsed model+task.
type SupToolMsg struct {
	ToolID   string
	Name     string
	Command  string
	Delegate *DelegateInfo
}

// DelegateInfo is a parsed `strawboss delegate` invocation.
type DelegateInfo struct {
	Model string
	Task  string
}

// SupToolResultMsg closes a tool call.
type SupToolResultMsg struct {
	ToolID  string
	Content string
	IsError bool
}

// SupStatusMsg drives the "✻ thinking…" line ("" clears it).
type SupStatusMsg struct{ Status string }

// SupUsageMsg accumulates supervisor token totals (per completed turn —
// the authoritative numbers from the result event; arriving, it replaces
// the live-turn estimate built from SupTurnUsageMsg).
type SupUsageMsg struct {
	Input, Output         int
	CacheRead, CacheWrite int
	CostUSD               float64
	Turns                 int // increment
}

// SupTurnUsageMsg is one API call's usage WITHIN the running turn (from
// each complete assistant message) — without it the supervisor counter
// would sit at zero for the whole length of a long turn.
type SupTurnUsageMsg struct {
	Input, Output         int
	CacheRead, CacheWrite int
}

// SupRateLimitMsg is live plan-window utilization (0..1).
type SupRateLimitMsg struct{ FiveHour, SevenDay float64 }

// SupTurnDoneMsg ends a turn (error text is displayed state).
type SupTurnDoneMsg struct {
	Err         string
	Interrupted bool
}

// WorkerUpsertMsg creates or updates a worker row.
type WorkerUpsertMsg struct {
	ID      string
	Model   string // model config name; "" keeps existing
	Task    string // "" keeps existing
	Status  string // queued/running/done/failed; "" keeps existing
	Summary string
	LogPath string
	Dir     string    // working directory; "" keeps existing
	Started time.Time // zero keeps existing
	Ended   time.Time // when the worker finished; zero means "now" on a
	// done/failed transition (live events) vs. the recorded time (replay)
}

// WorkerUsageMsg updates a worker's token counts.
type WorkerUsageMsg struct {
	ID            string
	Input, Output int
	// Ctx is the worker's current context footprint (last request's
	// prompt size incl. cache reads); 0 = unknown, keeps existing.
	Ctx int
}

// WorkerEventMsg appends a transcript line to the worker detail pane.
type WorkerEventMsg struct {
	ID   string
	Kind string // "tool", "text", "reasoning", "error"
	Text string
	// Replay: history repopulating a transcript after a restart — render
	// it, but keep it out of the logs tab (it did not just happen).
	Replay bool
}

// ModelStatMsg updates one model config's endpoint stats.
type ModelStatMsg struct {
	Name   string
	TokSec float64
	Active int
	Queue  int
	Note   string // e.g. "vllm · 2×GX10 split"
	// ContextWindow is the model's context length as the endpoint reports
	// it (sglang max_model_len); 0 = unknown.
	ContextWindow int
}

// RawLogMsg appends to the logs tab.
type RawLogMsg struct {
	Source string // "sup", "wrk", "app"
	Line   string
}

// ToastMsg shows a transient status line (no bell) — feedback for
// user-initiated worker actions.
type ToastMsg struct{ Text string }

// RemoteMsg announces that a remote control channel is armed (OpenClaw
// two-way): shown in the topbar so leaving the desk is an informed act.
type RemoteMsg struct{ Channel string }

// SessionInfo is one entry in the session picker (this project only).
type SessionInfo struct {
	ID      string
	Run     string
	Started time.Time
	Label   string // first prompt of the session
	Workers int
	Done    int
	Failed  int
	Current bool
}

// SessionSwitchedMsg announces a completed session switch: the UI resets
// chat and worker state; the conversation resumes on the next prompt.
type SessionSwitchedMsg struct{ ID string }

// SendPromptMsg is emitted BY the UI when the user submits input; the
// program driver (demo or live) subscribes and acts.
type SendPromptMsg struct{ Text string }

// InterruptMsg is emitted by the UI on esc.
type InterruptMsg struct{}

// KillWorkerMsg is emitted by the UI when the user asks to kill the
// selected running worker (dashboard `x`).
type KillWorkerMsg struct{ ID string }

// RetryWorkerMsg is emitted by the UI when the user asks to re-run the
// selected finished worker's task as a new worker (dashboard `r`).
type RetryWorkerMsg struct{ ID string }

// tickMsg drives the run clock, pulse animation, and toast expiry.
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(600*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// feedBatch carries every feed message already buffered when the
// listener woke: startup replay floods hundreds of msgs, and rendering
// them one per update cycle makes totals visibly "climb" while history
// loads. A batch folds in one render.
type feedBatch []tea.Msg

// Listen adapts a feed channel into Bubble Tea's message loop; Update
// re-arms it after every batch. It blocks for the first message, then
// greedily drains whatever else is pending (capped so a firehose can't
// starve keystrokes).
func Listen(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		batch := feedBatch{msg}
		for len(batch) < 512 {
			select {
			case more, ok := <-ch:
				if !ok {
					return batch
				}
				batch = append(batch, more)
			default:
				return batch
			}
		}
		return batch
	}
}
