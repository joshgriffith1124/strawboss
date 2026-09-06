// Package live wires the real feeds into the TUI: the supervisor driver's
// parsed stream, the worker registry file, and opencode polling — all
// emitted as ui msgs over one channel. Observation stays passive: nothing
// here injects a single token into the supervisor's context.
package live

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// usableSlashCommands returns the CLI's slash commands minus the
// terminal-only ones, sorted for the completion list.
func usableSlashCommands(all, terminalOnly []string) []string {
	skip := map[string]bool{}
	for _, c := range terminalOnly {
		skip[c] = true
	}
	var out []string
	for _, c := range all {
		if !skip[c] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// mapSupEvent translates one supervisor stream event into UI msgs.
// supModel is the session model from system/init: the result's modelUsage
// also lists the CLI's small internal helper, so the window has to be
// picked by name rather than guessed from the map.
func mapSupEvent(ev supervisor.Event, pid int, supModel string) []tea.Msg {
	switch e := ev.(type) {
	case supervisor.InitEvent:
		auth := "subscription"
		if e.APIKeySource != "none" {
			auth = "API KEY (" + e.APIKeySource + ") — invariant violated"
		}
		return []tea.Msg{ui.SupInitMsg{SessionID: e.SessionID, Model: e.Model, Auth: auth, PID: pid,
			SlashCommands: usableSlashCommands(e.SlashCommands, e.TerminalOnly)}}
	case supervisor.StatusEvent:
		switch {
		case e.Status == "requesting":
			return []tea.Msg{ui.SupStatusMsg{Status: "thinking…"}}
		case e.Status == "compacting":
			return []tea.Msg{ui.SupStatusMsg{Status: "compacting context…"}}
		case e.CompactResult == "failed":
			return []tea.Msg{ui.SupStatusMsg{Status: ""}, ui.SupNoteMsg{Text: "compact failed: " + e.CompactError, IsError: true}}
		case e.CompactResult != "":
			return []tea.Msg{ui.SupStatusMsg{Status: ""}, ui.SupNoteMsg{Text: "context compacted (" + e.CompactResult + ")"}}
		}
		return nil
	case supervisor.CommandLifecycleEvent:
		// A scheduled job (/loop) firing its own turn inside the process.
		return []tea.Msg{ui.RawLogMsg{Source: "sup", Line: "scheduled command " + e.State}}
	case supervisor.ThinkingEvent:
		return []tea.Msg{ui.SupStatusMsg{Status: fmt.Sprintf("thinking… ~%d tok", e.EstimatedTokens)}}
	case supervisor.StreamDeltaEvent:
		return []tea.Msg{ui.SupStatusMsg{Status: ""}, ui.SupTextDeltaMsg{Text: e.Text}}
	case supervisor.AssistantEvent:
		var msgs []tea.Msg
		if e.Usage.InputTokens+e.Usage.OutputTokens+e.Usage.CacheReadTokens+e.Usage.CacheCreationTokens > 0 {
			msgs = append(msgs, ui.SupTurnUsageMsg{
				Input:      e.Usage.InputTokens,
				Output:     e.Usage.OutputTokens,
				CacheRead:  e.Usage.CacheReadTokens,
				CacheWrite: e.Usage.CacheCreationTokens,
			})
		}
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
			if tu.Name == "CronCreate" {
				// /loop schedules a session-only cron job: it fires as long
				// as THIS process lives and vanishes with it (NOTES.md).
				msgs = append(msgs, ui.SupNoteMsg{Text: "scheduled job lives only in this supervisor process — " +
					"esc (interrupt), /new, or a crash drops it; re-issue the /loop after a respawn"})
			}
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
			CtxWindow:  e.ModelWindows[supModel],
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
		case toks[i] == "--task-file" && i+1 < len(toks):
			d.TaskFile = toks[i+1]
		case strings.HasPrefix(toks[i], "--task-file="):
			d.TaskFile = strings.TrimPrefix(toks[i], "--task-file=")
		}
	}
	if d.Model == "" && d.Task == "" && d.TaskFile == "" {
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
