package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"strawboss/internal/config"
	"strawboss/internal/harness"
)

// Harness implements harness.WorkerHarness against one `opencode serve`
// instance. A worker is an opencode session; the session id is the worker id.
type Harness struct {
	Client *Client
	// Dir is the working directory workers run in.
	Dir string
	// LogDir is where Result writes full transcripts (the terse-result
	// contract: the supervisor gets a path, never the transcript).
	LogDir string
	// PollInterval for Result's completion polling. Default 500ms.
	PollInterval time.Duration
	// StallAfter is how long an idle-looking session may go without a
	// record update before it's declared dead. Default 45s.
	StallAfter time.Duration
}

// New returns a Harness for the model config's endpoint (the opencode
// server base URL).
func New(mc config.ModelConfig, dir, logDir string) *Harness {
	return &Harness{
		Client: &Client{Base: strings.TrimRight(mc.Endpoint, "/")},
		Dir:    dir,
		LogDir: logDir,
	}
}

// splitModel splits a config model reference "provider/model" into opencode's
// providerID and modelID (e.g. "spark-a/qwen3.8-27b").
func splitModel(ref string) (providerID, modelID string, err error) {
	providerID, modelID, ok := strings.Cut(ref, "/")
	if !ok || providerID == "" || modelID == "" {
		return "", "", fmt.Errorf("model ref %q: want provider/model", ref)
	}
	return providerID, modelID, nil
}

// Spawn creates a session and fires the task at it without waiting.
func (h *Harness) Spawn(ctx context.Context, task string, model config.ModelConfig) (string, error) {
	providerID, modelID, err := splitModel(model.Model)
	if err != nil {
		return "", fmt.Errorf("spawning worker: %w", err)
	}
	id, err := h.Client.CreateSession(ctx, h.Dir, "")
	if err != nil {
		return "", fmt.Errorf("spawning worker: %w", err)
	}
	if err := h.Client.PromptAsync(ctx, id, providerID, modelID, model.Variant, task); err != nil {
		return "", fmt.Errorf("spawning worker %s: %w", id, err)
	}
	return id, nil
}

// Status maps opencode's live status (busy/retry, absent = idle) plus the
// transcript's error state onto the worker lifecycle.
func (h *Harness) Status(ctx context.Context, workerID string) (harness.Status, error) {
	statuses, err := h.Client.Status(ctx, h.Dir)
	if err != nil {
		return "", fmt.Errorf("worker %s status: %w", workerID, err)
	}
	if st, ok := statuses[workerID]; ok && st.Type != "idle" {
		return harness.StatusRunning, nil
	}
	msgs, err := h.Client.Messages(ctx, workerID)
	if err != nil {
		return "", fmt.Errorf("worker %s status: %w", workerID, err)
	}
	return finishedStatus(msgs), nil
}

// finishedStatus classifies a session from its transcript. An INCOMPLETE
// last assistant message reports as running — only its time.completed (or
// an error) proves the turn actually ended; classifying it done here once
// closed rows on workers that were mid-generation.
func finishedStatus(msgs []Message) harness.Status {
	last := lastAssistant(msgs)
	if last == nil {
		return harness.StatusQueued
	}
	if len(last.Info.Error) > 0 && string(last.Info.Error) != "null" {
		return harness.StatusFailed
	}
	for _, p := range last.Parts {
		if p.Type == "tool" && p.State.Status == "error" {
			return harness.StatusFailed
		}
	}
	if last.Info.Time.Completed == 0 {
		return harness.StatusRunning
	}
	return harness.StatusDone
}

func lastAssistant(msgs []Message) *Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Info.Role == "assistant" {
			return &msgs[i]
		}
	}
	return nil
}

// Events streams the worker's transcript as it happens: text/reasoning
// deltas and tool-call state changes. The channel closes when the session
// goes idle, the stream drops, or ctx is cancelled.
func (h *Harness) Events(ctx context.Context, workerID string) (<-chan harness.Event, error) {
	ctx, cancel := context.WithCancel(ctx)
	src, err := h.Client.Events(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("worker %s events: %w", workerID, err)
	}
	out := make(chan harness.Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		for ev := range src {
			if ev.Properties.SessionID != workerID {
				continue
			}
			var he harness.Event
			switch ev.Type {
			case "message.part.delta":
				if ev.Properties.Field != "text" && ev.Properties.Field != "reasoning" {
					continue
				}
				he = harness.Event{Time: time.Now(), Kind: ev.Properties.Field, Text: ev.Properties.Delta}
			case "message.part.updated":
				p := ev.Properties.Part
				if p == nil || p.Type != "tool" {
					continue
				}
				if p.State.Status != "completed" && p.State.Status != "error" && p.State.Status != "running" {
					continue
				}
				kind := "tool"
				if p.State.Status == "error" {
					kind = "error"
				}
				he = harness.Event{Time: time.Now(), Kind: kind,
					Text: fmt.Sprintf("%s %s [%s]", p.Tool, p.State.Title, p.State.Status)}
			case "session.idle":
				return
			default:
				continue
			}
			select {
			case out <- he:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Usage reports the session's cumulative token counts.
func (h *Harness) Usage(ctx context.Context, workerID string) (harness.Usage, error) {
	info, err := h.Client.SessionInfo(ctx, workerID)
	if err != nil {
		return harness.Usage{}, fmt.Errorf("worker %s usage: %w", workerID, err)
	}
	return harness.Usage{
		InputTokens:  info.Tokens.Input + info.Tokens.Cache.Read + info.Tokens.Cache.Write,
		OutputTokens: info.Tokens.Output + info.Tokens.Reasoning,
	}, nil
}

// maxSummaryBytes caps the terse summary; the supervisor-channel budget is
// ~250 tokens for the whole result (CLAUDE.md invariant 3).
const maxSummaryBytes = 700

// Result blocks until the worker finishes, writes the full transcript to
// LogDir, and returns the terse result for the supervisor channel.
func (h *Harness) Result(ctx context.Context, workerID string) (harness.Result, error) {
	interval := h.PollInterval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	stallAfter := h.StallAfter
	if stallAfter == 0 {
		stallAfter = 45 * time.Second
	}
	// prompt_async admits the task before the session shows busy, so a
	// too-early idle is not "finished" — it's "not started yet". Finished
	// means the transcript proves it: a completed (or errored) final
	// assistant message. opencode can also die MID-message with no error
	// recorded (seen live — docs/NOTES.md), so a session that goes idle
	// with an incomplete message, or whose record stops updating, is
	// declared stalled rather than waited on forever. ctx bounds the wait.
	started := false
	stalled := false
	idlePolls := 0
	for !stalled {
		statuses, err := h.Client.Status(ctx, h.Dir)
		if err != nil {
			return harness.Result{}, fmt.Errorf("worker %s result: %w", workerID, err)
		}
		if st, ok := statuses[workerID]; ok && st.Type != "idle" {
			started = true
			idlePolls = 0
		} else {
			idlePolls++
			msgs, err := h.Client.Messages(ctx, workerID)
			if err != nil {
				return harness.Result{}, fmt.Errorf("worker %s result: %w", workerID, err)
			}
			if st := finishedStatus(msgs); st == harness.StatusDone || st == harness.StatusFailed {
				break
			}
			if started && idlePolls >= 2 {
				// Was running, now idle, transcript incomplete: died mid-message.
				stalled = true
				break
			}
			if !started {
				// Never seen busy (or died before the first poll): fall back
				// to the session record's last-updated time.
				info, err := h.Client.SessionInfo(ctx, workerID)
				if err == nil && info.Time.Updated > 0 &&
					time.Since(time.UnixMilli(info.Time.Updated)) > stallAfter {
					stalled = true
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return harness.Result{}, fmt.Errorf("worker %s result: %w", workerID, ctx.Err())
		case <-time.After(interval):
		}
	}

	msgs, err := h.Client.Messages(ctx, workerID)
	if err != nil {
		return harness.Result{}, fmt.Errorf("worker %s result: %w", workerID, err)
	}
	logPath, err := h.writeLog(workerID, msgs)
	if err != nil {
		return harness.Result{}, err
	}
	status := finishedStatus(msgs)
	summary := summarize(msgs)
	if stalled || status == harness.StatusRunning || status == harness.StatusQueued {
		status = harness.StatusFailed
		summary = "worker stopped without completing its reply (no error recorded by opencode); partial transcript in log. " + summary
	}
	// A "clean" completion that is pure reasoning — no answer, no tool
	// calls — means the model exhausted its output budget thinking (seen
	// live: 49k chars of reasoning, output tokens pegged at the limit).
	// Report failure with advice, or the supervisor retries the same task
	// into the same wall forever.
	if status == harness.StatusDone {
		if last := lastAssistant(msgs); last != nil && reasoningOnly(last) {
			status = harness.StatusFailed
			summary = fmt.Sprintf("worker produced only internal reasoning and no answer — its output budget (%d tokens) was likely exhausted thinking. Do NOT retry the same task: split it into smaller pieces or demand a much smaller deliverable.", last.Info.Tokens.Output)
		}
	}
	return harness.Result{
		WorkerID: workerID,
		Status:   status,
		Summary:  summary,
		LogPath:  logPath,
	}, nil
}

// summarize builds the few-line summary: the final assistant text, falling
// back to the last text anywhere in the transcript, then to a tool recap —
// "(empty reply)" starves the supervisor of exactly what it needs.
func summarize(msgs []Message) string {
	last := lastAssistant(msgs)
	if last == nil {
		return "(no assistant reply)"
	}
	text := textOf(last)
	if text == "" {
		for i := len(msgs) - 1; i >= 0 && text == ""; i-- {
			if msgs[i].Info.Role == "assistant" {
				text = textOf(&msgs[i])
			}
		}
		if text != "" {
			text = "(no final reply; last text was:) " + text
		}
	}
	if text == "" {
		tools := 0
		lastTool := ""
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == "tool" {
					tools++
					lastTool = strings.TrimSpace(p.Tool + " " + p.State.Title)
				}
			}
		}
		if tools > 0 {
			text = fmt.Sprintf("(no reply text; ran %d tool steps, last: %s — check the log)", tools, lastTool)
		} else {
			text = "(empty reply)"
		}
	}
	if len(last.Info.Error) > 0 && string(last.Info.Error) != "null" {
		text = strings.TrimSpace("worker error: " + compactError(last.Info.Error) + "\n" + text)
	}
	if len(text) > maxSummaryBytes {
		text = text[:maxSummaryBytes] + "…"
	}
	return text
}

// reasoningOnly reports a message that thought but never answered: at
// least one reasoning part, and neither text nor tool parts.
func reasoningOnly(m *Message) bool {
	sawReasoning := false
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) != "" {
				return false
			}
		case "tool":
			return false
		case "reasoning":
			sawReasoning = true
		}
	}
	return sawReasoning
}

// textOf concatenates a message's text parts.
func textOf(m *Message) string {
	var text string
	for _, p := range m.Parts {
		if p.Type == "text" {
			text += p.Text
		}
	}
	return strings.TrimSpace(text)
}

func compactError(raw json.RawMessage) string {
	var e struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && (e.Name != "" || e.Data.Message != "") {
		return strings.TrimSpace(e.Name + " " + e.Data.Message)
	}
	s := string(raw)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// writeLog dumps the full transcript as JSONL (one message per line) so the
// TUI and the supervisor's pay-per-use Read both have somewhere to go.
func (h *Harness) writeLog(workerID string, msgs []Message) (string, error) {
	dir := h.LogDir
	if dir == "" {
		base, err := config.DefaultStateDir()
		if err != nil {
			return "", fmt.Errorf("worker %s log: %w", workerID, err)
		}
		dir = filepath.Join(base, "logs")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("worker %s log: %w", workerID, err)
	}
	path := filepath.Join(dir, workerID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("worker %s log: %w", workerID, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return "", fmt.Errorf("worker %s log: %w", workerID, err)
		}
	}
	return path, nil
}

// Abort stops a running worker (used by the runner on timeout/interrupt
// and by the TUI's worker kill; not part of WorkerHarness).
func (h *Harness) Abort(ctx context.Context, workerID string) error {
	if err := h.Client.Abort(ctx, workerID); err != nil {
		return fmt.Errorf("aborting worker %s: %w", workerID, err)
	}
	return nil
}

var _ harness.WorkerHarness = (*Harness)(nil)
