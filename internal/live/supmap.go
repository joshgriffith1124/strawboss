// Package live wires the real feeds into the TUI: the supervisor driver's
// parsed stream, the worker registry file, and opencode polling — all
// emitted as ui msgs over one channel. Observation stays passive: nothing
// here injects a single token into the supervisor's context.
package live

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/supervisor"
	"strawboss/internal/ui"
)

// mapSupEvent translates one supervisor stream event into UI msgs.
func mapSupEvent(ev supervisor.Event, pid int) []tea.Msg {
	switch e := ev.(type) {
	case supervisor.InitEvent:
		auth := "subscription"
		if e.APIKeySource != "none" {
			auth = "API KEY (" + e.APIKeySource + ") — invariant violated"
		}
		return []tea.Msg{ui.SupInitMsg{SessionID: e.SessionID, Model: e.Model, Auth: auth, PID: pid}}
	case supervisor.StatusEvent:
		if e.Status == "requesting" {
			return []tea.Msg{ui.SupStatusMsg{Status: "thinking…"}}
		}
		return nil
	case supervisor.ThinkingEvent:
		return []tea.Msg{ui.SupStatusMsg{Status: fmt.Sprintf("thinking… ~%d tok", e.EstimatedTokens)}}
	case supervisor.StreamDeltaEvent:
		return []tea.Msg{ui.SupStatusMsg{Status: ""}, ui.SupTextDeltaMsg{Text: e.Text}}
	case supervisor.AssistantEvent:
		var msgs []tea.Msg
		if strings.TrimSpace(e.Text) != "" {
			ts := e.Timestamp
			if ts.IsZero() {
				ts = time.Now()
			}
			msgs = append(msgs, ui.SupTextDoneMsg{Text: e.Text, Time: ts})
		}
		for _, tu := range e.ToolUses {
			cmd, _ := tu.BashCommand()
			msgs = append(msgs, ui.SupToolMsg{
				ToolID:   tu.ID,
				Name:     tu.Name,
				Command:  cmd,
				Delegate: parseDelegate(cmd),
			})
		}
		return msgs
	case supervisor.ToolResultsEvent:
		var msgs []tea.Msg
		for _, r := range e.Results {
			msgs = append(msgs, ui.SupToolResultMsg{ToolID: r.ToolUseID, Content: r.Content, IsError: r.IsError})
		}
		return msgs
	case supervisor.RateLimitEvent:
		return []tea.Msg{ui.SupRateLimitMsg{FiveHour: e.FiveHour.Utilization, SevenDay: e.SevenDay.Utilization}}
	case supervisor.ResultEvent:
		// The result closes a turn; SupTurnDoneMsg (no error) flushes any
		// streaming text and clears the status line. In stream mode the
		// process outlives the turn, so this is the only turn boundary.
		return []tea.Msg{ui.SupUsageMsg{
			Input:      e.Usage.InputTokens,
			Output:     e.Usage.OutputTokens,
			CacheRead:  e.Usage.CacheReadTokens,
			CacheWrite: e.Usage.CacheCreationTokens,
			CostUSD:    e.TotalCostUSD,
			Turns:      1,
		}, ui.SupTurnDoneMsg{}}
	case supervisor.UnknownEvent:
		if e.Type != "" {
			return []tea.Msg{ui.RawLogMsg{Source: "sup", Line: "unparsed " + e.Type + " event"}}
		}
		return nil
	case supervisor.TurnDoneEvent:
		errText := ""
		if e.ExitErr != nil {
			errText = e.ExitErr.Error()
			if s := strings.TrimSpace(e.Stderr); s != "" {
				if len(s) > 300 {
					s = s[:300] + "…"
				}
				errText += ": " + s
			}
		}
		return []tea.Msg{ui.SupTurnDoneMsg{Err: errText, Interrupted: e.Interrupted}}
	}
	return nil
}

// parseDelegate recognizes a `strawboss delegate` Bash command and pulls
// out --model and --task so the chat can show the terse delegation line.
// Returns nil for anything else.
func parseDelegate(cmd string) *ui.DelegateInfo {
	if !strings.Contains(cmd, "strawboss") || !strings.Contains(cmd, "delegate") {
		return nil
	}
	toks := shellTokens(cmd)
	seen := false
	for i, t := range toks {
		if t == "delegate" && i > 0 && strings.HasSuffix(toks[i-1], "strawboss") {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	d := &ui.DelegateInfo{}
	for i := 0; i < len(toks); i++ {
		switch {
		case toks[i] == "--model" && i+1 < len(toks):
			d.Model = toks[i+1]
		case strings.HasPrefix(toks[i], "--model="):
			d.Model = strings.TrimPrefix(toks[i], "--model=")
		case toks[i] == "--task" && i+1 < len(toks):
			d.Task = toks[i+1]
		case strings.HasPrefix(toks[i], "--task="):
			d.Task = strings.TrimPrefix(toks[i], "--task=")
		}
	}
	if d.Model == "" && d.Task == "" {
		return nil
	}
	return d
}

// shellTokens splits a command respecting single/double quotes (enough for
// the delegate invocations the supervisor writes; not a full shell parser).
func shellTokens(s string) []string {
	var toks []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}
