// Package opencode implements harness.WorkerHarness against a local
// `opencode serve` HTTP API (verified against opencode 1.18.25 — see
// docs/NOTES.md). opencode runs on this machine; the model configs it
// serves point at the local inference endpoints.
package opencode

import "encoding/json"

// Tokens is opencode's token accounting (per message or per session).
type Tokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// SessionInfo is the session record from GET /api/session/{id}; Tokens here
// are cumulative for the session.
type SessionInfo struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Title  string `json:"title"`
	Cost   float64
	Tokens Tokens `json:"tokens"`
	Time   struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// SessionStatus is one entry of GET /session/status. Sessions not in the
// map are idle.
type SessionStatus struct {
	Type string `json:"type"` // "busy", "retry", "idle"
}

// ToolState is the state of a tool Part as it runs.
type ToolState struct {
	Status   string          `json:"status"` // "pending", "running", "completed", "error"
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Title    string          `json:"title"`
	Metadata json.RawMessage `json:"metadata"`
}

// Part is one piece of a message: text, reasoning, a tool call, step
// markers, etc.
type Part struct {
	ID        string    `json:"id"`
	MessageID string    `json:"messageID"`
	SessionID string    `json:"sessionID"`
	Type      string    `json:"type"` // "text", "reasoning", "tool", "step-start", "step-finish"
	Text      string    `json:"text"`
	Tool      string    `json:"tool"`
	State     ToolState `json:"state"`
}

// MessageInfo is the metadata half of a message.
type MessageInfo struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"` // "user", "assistant"
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Tokens     Tokens `json:"tokens"`
	Time       struct {
		Created int64 `json:"created"`
		// Completed is 0 while the message is still being generated.
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error json.RawMessage `json:"error"` // non-empty when the message failed
}

// Message is one transcript entry from GET /session/{id}/message.
type Message struct {
	Info  MessageInfo `json:"info"`
	Parts []Part      `json:"parts"`
}

// ServerEvent is one SSE event from GET /global/event (unwrapped from its
// {directory, payload} envelope). Properties varies by Type; the fields
// below cover the types the harness consumes.
type ServerEvent struct {
	Type       string `json:"type"`
	Directory  string `json:"-"` // from the global envelope
	Properties struct {
		SessionID string          `json:"sessionID"`
		MessageID string          `json:"messageID"`
		PartID    string          `json:"partID"`
		Field     string          `json:"field"` // for message.part.delta
		Delta     string          `json:"delta"`
		Part      *Part           `json:"part"`   // for message.part.updated
		Status    *SessionStatus  `json:"status"` // for session.status
		Raw       json.RawMessage `json:"-"`
	} `json:"properties"`
}
