package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The feed vocabulary. Every external source (supervisor driver, harness
// poller, replay) emits these; the UI knows nothing about where they come
// from. M5 maps real supervisor/harness events onto them.

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

// SupUsageMsg accumulates supervisor token totals (per completed turn).
type SupUsageMsg struct {
	Input, Output         int
	CacheRead, CacheWrite int
	CostUSD               float64
	Turns                 int // increment
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
	Started time.Time // zero keeps existing
	Ended   time.Time // when the worker finished; zero means "now" on a
	// done/failed transition (live events) vs. the recorded time (replay)
}

// WorkerUsageMsg updates a worker's token counts.
type WorkerUsageMsg struct {
	ID            string
	Input, Output int
}

// WorkerEventMsg appends a transcript line to the worker detail pane.
type WorkerEventMsg struct {
	ID   string
	Kind string // "tool", "text", "reasoning", "error"
	Text string
}

// ModelStatMsg updates one model config's endpoint stats.
type ModelStatMsg struct {
	Name   string
	TokSec float64
	Active int
	Queue  int
	Note   string // e.g. "vllm · 2×GX10 split"
}

// RawLogMsg appends to the logs tab.
type RawLogMsg struct {
	Source string // "sup", "wrk", "app"
	Line   string
}

// ToastMsg shows a transient status line (no bell) — feedback for
// user-initiated worker actions.
type ToastMsg struct{ Text string }

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

// Listen adapts a feed channel into Bubble Tea's message loop; Update
// re-arms it after every feed message.
func Listen(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
