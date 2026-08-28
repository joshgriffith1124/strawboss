// Package supervisor spawns and drives the Claude Code CLI in headless mode
// (`claude -p --output-format stream-json`) on subscription auth, parsing its
// stdout into typed events. This stream IS the TUI's supervisor data feed:
// observation is passive and adds zero tokens to the supervisor's context.
package supervisor

import (
	"encoding/json"
	"time"
)

// Event is one parsed line of the claude stream-json output. Concrete types:
// InitEvent, StatusEvent, AssistantEvent, ToolResultsEvent, StreamDeltaEvent,
// RateLimitEvent, ResultEvent, UnknownEvent, TurnDoneEvent.
type Event interface {
	event()
}

// Usage is per-message token accounting from the stream's usage fields.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// Total is all input-side tokens that entered context for the message.
func (u Usage) Total() int {
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
}

// Add accumulates another message's usage.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:         u.InputTokens + v.InputTokens,
		OutputTokens:        u.OutputTokens + v.OutputTokens,
		CacheCreationTokens: u.CacheCreationTokens + v.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens + v.CacheReadTokens,
	}
}

// InitEvent is the system/init line that opens every turn. SessionID from the
// first turn is what --resume needs on later turns.
type InitEvent struct {
	SessionID      string
	Model          string
	PermissionMode string
	// APIKeySource must be "none" for subscription auth — anything else means
	// an API key leaked into the subprocess env (invariant 1).
	APIKeySource      string
	ClaudeCodeVersion string
	CWD               string
	Tools             []string
}

// StatusEvent is a system/status line ("requesting", ...).
type StatusEvent struct {
	SessionID string
	Status    string
}

// ThinkingEvent is a system/thinking_tokens line: a live estimate of the
// current thinking pass, emitted periodically while the model reasons.
type ThinkingEvent struct {
	SessionID       string
	EstimatedTokens int
}

// ToolUse is one tool_use content block from an assistant message. Delegation
// detection lives here: a Bash tool_use invoking the delegate command is the
// moment a worker row appears in the TUI.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// BashCommand returns the command string if this is a Bash tool call.
func (t ToolUse) BashCommand() (string, bool) {
	if t.Name != "Bash" {
		return "", false
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(t.Input, &in); err != nil || in.Command == "" {
		return "", false
	}
	return in.Command, true
}

// AssistantEvent is a complete assistant message: text and/or tool calls,
// with that message's token usage.
type AssistantEvent struct {
	SessionID string
	MessageID string
	Text      string // concatenated text blocks
	Thinking  string // concatenated thinking blocks
	ToolUses  []ToolUse
	Usage     Usage
	Timestamp time.Time
}

// ToolResult is one tool_result block from a user message; for a delegation,
// its Content is the terse result the worker handed back.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ToolResultsEvent is a user message carrying tool results back to the model.
type ToolResultsEvent struct {
	SessionID string
	Results   []ToolResult
	Timestamp time.Time
}

// StreamDeltaEvent is a partial text chunk (from --include-partial-messages)
// for live-streaming the assistant's reply in the chat pane.
type StreamDeltaEvent struct {
	SessionID string
	Text      string
}

// RateLimitWindow is one plan window's live state.
type RateLimitWindow struct {
	Utilization float64 // 0..1
	ResetsAt    time.Time
}

// RateLimitEvent carries live plan-window utilization (five-hour and
// seven-day) — the CLI does report this, contrary to the kickoff assumption.
type RateLimitEvent struct {
	SessionID string
	Status    string
	FiveHour  RateLimitWindow
	SevenDay  RateLimitWindow
}

// ResultEvent closes a turn: final result text, cumulative usage for the
// turn, and the notional API cost (marginal real cost is $0.00 on
// subscription).
type ResultEvent struct {
	SessionID     string
	Subtype       string // "success", ...
	IsError       bool
	Result        string
	TotalCostUSD  float64
	NumTurns      int
	Usage         Usage
	DurationMS    int
	DurationAPIMS int
}

// UnknownEvent preserves lines this parser doesn't understand (new CLI
// versions, malformed JSON). A weird line is displayed state, never a crash.
type UnknownEvent struct {
	Type string
	Raw  []byte
	Err  error
}

// TurnDoneEvent is emitted by the driver (not parsed from the stream) as the
// final event of a turn, after the subprocess exits.
type TurnDoneEvent struct {
	// ExitErr is non-nil if the claude process exited abnormally.
	ExitErr error
	// Stderr is the subprocess's captured stderr (usually empty).
	Stderr string
	// Interrupted is true when the turn ended because Interrupt was called.
	Interrupted bool
}

func (InitEvent) event()        {}
func (StatusEvent) event()      {}
func (ThinkingEvent) event()    {}
func (AssistantEvent) event()   {}
func (ToolResultsEvent) event() {}
func (StreamDeltaEvent) event() {}
func (RateLimitEvent) event()   {}
func (ResultEvent) event()      {}
func (UnknownEvent) event()     {}
func (TurnDoneEvent) event()    {}
