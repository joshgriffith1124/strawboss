package dshacp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"strawboss/internal/harness"
)

// The dsh session log (`persistenceCompression: none`) is the worker's
// whole observable life: assistant chunks, tool calls/results, per-request
// usage, turn boundaries. The ACP wire deliberately carries none of that,
// so both the TUI transcript and Usage/Result detail come from here.

// sessionEvent is one line of session.jsonl.
type sessionEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type chunkData struct {
	Chunk struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Name   string `json:"name"`
		Usage  struct{ InputTokens, OutputTokens, CacheReadTokens int }
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	} `json:"chunk"`
}

type toolCallData struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type messageData struct {
	Message struct {
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			IsError bool   `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	} `json:"message"`
	Error json.RawMessage `json:"error"`
}

type turnEndData struct {
	Reason struct {
		Kind string `json:"kind"`
	} `json:"reason"`
}

// TailItem is one observed change in a worker's session log.
type TailItem struct {
	Event     *harness.Event // transcript line/delta; nil for usage/turn items
	Usage     *harness.Usage // cumulative usage, present when it changed
	TurnEnded bool
	EndReason string // turn/end reason kind, e.g. "completed"
	// Replay marks items parsed from content that already existed when the
	// tail began — history repopulating a transcript, not live activity.
	// Consumers should render but not log or alert on these.
	Replay bool

	at int64 // byte offset of the source line, for the replay boundary
}

// SessionInfo is the folded state of a session log.
type SessionInfo struct {
	Usage        harness.Usage
	LastText     string // last assistant text block seen
	FinishReason string // last model finish reason kind, e.g. "stop", "length"
	TurnEnded    bool
	EndReason    string
}

// FindSessionLog locates a session's JSONL under the persistence root.
// dsh nests logs as <persistenceRoot>/<mangled-cwd>/<sessionID>/
// session.jsonl, and each worker gets its own persistence subtree under
// the shared root (SQLite lock contention — see Spawn), so the log sits
// one OR two levels down. The session id is globally unique: glob for it
// rather than reimplementing the cwd mangling (docs/NOTES.md).
func FindSessionLog(root, sessionID string) (string, error) {
	for _, pattern := range []string{
		filepath.Join(root, "*", "*", sessionID, "session.jsonl"),
		filepath.Join(root, "*", sessionID, "session.jsonl"),
	} {
		if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
			return matches[0], nil
		}
	}
	return "", fmt.Errorf("no session log for %s under %s", sessionID, root)
}

// parseLine turns one session.jsonl line into tail items (usually 0 or 1;
// a line can carry both a transcript event and a usage change).
func parseLine(line []byte, cum *harness.Usage, info *SessionInfo) []TailItem {
	var ev sessionEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "assistant/chunk":
		var d chunkData
		if json.Unmarshal(ev.Data, &d) != nil {
			return nil
		}
		switch d.Chunk.Type {
		case "text-delta":
			return []TailItem{{Event: &harness.Event{Time: time.Now(), Kind: "text", Text: d.Chunk.Text}}}
		case "reasoning-delta", "reasoning":
			return []TailItem{{Event: &harness.Event{Time: time.Now(), Kind: "reasoning", Text: d.Chunk.Text}}}
		case "usage":
			// Cache reads count as input — the same accounting opencode
			// session totals use, so the TUI token economy compares.
			cum.InputTokens += d.Chunk.Usage.InputTokens + d.Chunk.Usage.CacheReadTokens
			cum.OutputTokens += d.Chunk.Usage.OutputTokens
			u := *cum
			if info != nil {
				info.Usage = u
			}
			return []TailItem{{Usage: &u}}
		case "finish":
			if info != nil {
				info.FinishReason = d.Chunk.Reason.Kind
			}
		case "block-end":
			// The completed text block is authoritative for the summary
			// fallback; deltas already streamed it for the transcript.
			var d struct {
				Chunk struct {
					Block struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"block"`
				} `json:"chunk"`
			}
			if info != nil && json.Unmarshal(ev.Data, &d) == nil &&
				d.Chunk.Block.Type == "text" && strings.TrimSpace(d.Chunk.Block.Text) != "" {
				info.LastText = d.Chunk.Block.Text
			}
		}
	case "tool/call":
		var d toolCallData
		if json.Unmarshal(ev.Data, &d) != nil {
			return nil
		}
		args := d.Arguments
		if len(args) > 160 {
			args = args[:160] + "…"
		}
		return []TailItem{{Event: &harness.Event{Time: time.Now(), Kind: "tool", Text: d.Name + " " + args}}}
	case "tool/result":
		var d messageData
		if json.Unmarshal(ev.Data, &d) != nil {
			return nil
		}
		for _, c := range d.Message.Content {
			if c.Type != "tool-result" || !c.IsError {
				continue
			}
			text := "tool error"
			if len(c.Content) > 0 {
				text = c.Content[0].Text
			}
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			return []TailItem{{Event: &harness.Event{Time: time.Now(), Kind: "error", Text: text}}}
		}
	case "turn/end":
		var d turnEndData
		_ = json.Unmarshal(ev.Data, &d)
		if info != nil {
			info.TurnEnded, info.EndReason = true, d.Reason.Kind
		}
		return []TailItem{{TurnEnded: true, EndReason: d.Reason.Kind}}
	}
	return nil
}

// ReadSession folds a whole session log into its current state.
func ReadSession(path string) (SessionInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("reading session log: %w", err)
	}
	defer f.Close()
	var info SessionInfo
	var cum harness.Usage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		parseLine(sc.Bytes(), &cum, &info)
	}
	if err := sc.Err(); err != nil {
		return info, fmt.Errorf("reading session log: %w", err)
	}
	return info, nil
}

// TailSession follows a session log as it grows, emitting transcript
// events and usage updates. The channel closes when ctx is cancelled or
// after the turn ends (with a short grace read for trailing lines). The
// log file may not exist yet at call time — the tailer waits for it.
func TailSession(ctx context.Context, root, sessionID string, interval time.Duration) <-chan TailItem {
	if interval <= 0 {
		interval = 400 * time.Millisecond
	}
	out := make(chan TailItem, 64)
	go func() {
		defer close(out)
		var path string
		var offset int64
		var initialSize int64 = -1
		var cum harness.Usage
		ended := false
		for {
			if path == "" {
				path, _ = FindSessionLog(root, sessionID)
				if path != "" {
					if fi, err := os.Stat(path); err == nil {
						initialSize = fi.Size()
					}
				}
			}
			if path != "" {
				var items []TailItem
				items, offset = readFrom(path, offset, &cum)
				for i := range items {
					items[i].Replay = items[i].at < initialSize
				}
				for _, it := range items {
					if it.TurnEnded {
						ended = true
					}
					select {
					case out <- it:
					case <-ctx.Done():
						return
					}
				}
				if ended {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
	return out
}

// readFrom parses complete lines appended since offset; an unfinished
// trailing line stays unconsumed (the offset is not advanced past it) and
// is retried whole on the next poll.
func readFrom(path string, offset int64, cum *harness.Usage) ([]TailItem, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset
	}
	var items []TailItem
	rd := bufio.NewReader(f)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return items, offset
		}
		lineStart := offset
		offset += int64(len(line))
		for _, it := range parseLine(line, cum, nil) {
			it.at = lineStart
			items = append(items, it)
		}
	}
}
