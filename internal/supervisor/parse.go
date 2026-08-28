package supervisor

import (
	"encoding/json"
	"fmt"
	"time"
)

// ParseLine parses one line of `claude -p --output-format stream-json`
// output into a typed Event. It never fails hard: lines it can't understand
// come back as UnknownEvent (nil error), and a nil, nil return means the
// line carries nothing the TUI cares about (e.g. non-text stream deltas).
func ParseLine(line []byte) (Event, error) {
	var env struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return UnknownEvent{Raw: line, Err: fmt.Errorf("parsing stream line: %w", err)}, nil
	}

	switch env.Type {
	case "system":
		return parseSystem(line, env.Subtype, env.SessionID)
	case "assistant":
		return parseAssistant(line, env.SessionID)
	case "user":
		return parseUser(line, env.SessionID)
	case "stream_event":
		return parseStreamEvent(line, env.SessionID)
	case "rate_limit_event":
		return parseRateLimit(line, env.SessionID)
	case "result":
		return parseResult(line, env.SessionID)
	default:
		return UnknownEvent{Type: env.Type, Raw: line}, nil
	}
}

func parseSystem(line []byte, subtype, sessionID string) (Event, error) {
	switch subtype {
	case "init":
		var v struct {
			Model             string   `json:"model"`
			PermissionMode    string   `json:"permissionMode"`
			APIKeySource      string   `json:"apiKeySource"`
			ClaudeCodeVersion string   `json:"claude_code_version"`
			CWD               string   `json:"cwd"`
			Tools             []string `json:"tools"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			return UnknownEvent{Type: "system/init", Raw: line, Err: err}, nil
		}
		return InitEvent{
			SessionID:         sessionID,
			Model:             v.Model,
			PermissionMode:    v.PermissionMode,
			APIKeySource:      v.APIKeySource,
			ClaudeCodeVersion: v.ClaudeCodeVersion,
			CWD:               v.CWD,
			Tools:             v.Tools,
		}, nil
	case "status":
		var v struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			return UnknownEvent{Type: "system/status", Raw: line, Err: err}, nil
		}
		return StatusEvent{SessionID: sessionID, Status: v.Status}, nil
	case "thinking_tokens":
		var v struct {
			EstimatedTokens int `json:"estimated_tokens"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, nil // periodic chatter; never worth an unknown-event log
		}
		return ThinkingEvent{SessionID: sessionID, EstimatedTokens: v.EstimatedTokens}, nil
	default:
		return UnknownEvent{Type: "system/" + subtype, Raw: line}, nil
	}
}

// contentBlock covers the block variants inside message.content.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	// tool_result fields
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func parseAssistant(line []byte, sessionID string) (Event, error) {
	var v struct {
		Message struct {
			ID      string         `json:"id"`
			Content []contentBlock `json:"content"`
			Usage   Usage          `json:"usage"`
		} `json:"message"`
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return UnknownEvent{Type: "assistant", Raw: line, Err: err}, nil
	}
	ev := AssistantEvent{
		SessionID: sessionID,
		MessageID: v.Message.ID,
		Usage:     v.Message.Usage,
		Timestamp: v.Timestamp,
	}
	for _, b := range v.Message.Content {
		switch b.Type {
		case "text":
			ev.Text += b.Text
		case "thinking":
			ev.Thinking += b.Thinking
		case "tool_use":
			ev.ToolUses = append(ev.ToolUses, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return ev, nil
}

// toolResultText flattens tool_result content, which the API allows as
// either a plain string or a list of typed blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out
	}
	return string(raw)
}

func parseUser(line []byte, sessionID string) (Event, error) {
	var v struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return UnknownEvent{Type: "user", Raw: line, Err: err}, nil
	}
	// content is either a plain string (user text on resume — not feed
	// material) or a block list that may carry tool results.
	var blocks []contentBlock
	if err := json.Unmarshal(v.Message.Content, &blocks); err != nil {
		return nil, nil
	}
	ev := ToolResultsEvent{SessionID: sessionID, Timestamp: v.Timestamp}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			ev.Results = append(ev.Results, ToolResult{
				ToolUseID: b.ToolUseID,
				Content:   toolResultText(b.Content),
				IsError:   b.IsError,
			})
		}
	}
	if len(ev.Results) == 0 {
		// A user line with no tool results (e.g. injected user text on
		// resume) isn't something the feed needs.
		return nil, nil
	}
	return ev, nil
}

func parseStreamEvent(line []byte, sessionID string) (Event, error) {
	var v struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return UnknownEvent{Type: "stream_event", Raw: line, Err: err}, nil
	}
	if v.Event.Type == "content_block_delta" && v.Event.Delta.Type == "text_delta" {
		return StreamDeltaEvent{SessionID: sessionID, Text: v.Event.Delta.Text}, nil
	}
	// Other stream minutiae (block starts/stops, json deltas) add nothing
	// over the complete assistant event that follows.
	return nil, nil
}

func parseRateLimit(line []byte, sessionID string) (Event, error) {
	var v struct {
		Info struct {
			Status  string `json:"status"`
			Windows struct {
				FiveHour struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"five_hour"`
				SevenDay struct {
					Utilization float64 `json:"utilization"`
					ResetsAt    int64   `json:"resetsAt"`
				} `json:"seven_day"`
			} `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return UnknownEvent{Type: "rate_limit_event", Raw: line, Err: err}, nil
	}
	return RateLimitEvent{
		SessionID: sessionID,
		Status:    v.Info.Status,
		FiveHour: RateLimitWindow{
			Utilization: v.Info.Windows.FiveHour.Utilization,
			ResetsAt:    time.Unix(v.Info.Windows.FiveHour.ResetsAt, 0),
		},
		SevenDay: RateLimitWindow{
			Utilization: v.Info.Windows.SevenDay.Utilization,
			ResetsAt:    time.Unix(v.Info.Windows.SevenDay.ResetsAt, 0),
		},
	}, nil
}

func parseResult(line []byte, sessionID string) (Event, error) {
	var v struct {
		Subtype       string  `json:"subtype"`
		IsError       bool    `json:"is_error"`
		Result        string  `json:"result"`
		TotalCostUSD  float64 `json:"total_cost_usd"`
		NumTurns      int     `json:"num_turns"`
		Usage         Usage   `json:"usage"`
		DurationMS    int     `json:"duration_ms"`
		DurationAPIMS int     `json:"duration_api_ms"`
	}
	if err := json.Unmarshal(line, &v); err != nil {
		return UnknownEvent{Type: "result", Raw: line, Err: err}, nil
	}
	return ResultEvent{
		SessionID:     sessionID,
		Subtype:       v.Subtype,
		IsError:       v.IsError,
		Result:        v.Result,
		TotalCostUSD:  v.TotalCostUSD,
		NumTurns:      v.NumTurns,
		Usage:         v.Usage,
		DurationMS:    v.DurationMS,
		DurationAPIMS: v.DurationAPIMS,
	}, nil
}
