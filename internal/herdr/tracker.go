package herdr

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Token names reported to herdr. Show them in herdr's sidebar with e.g.
//
//	[ui.sidebar.agents.rows_by_agent]
//	strawboss = [["state_icon", "workspace", "tab"], ["$workers", "$local_tok"]]
var tokenNames = []string{"workers", "local_tok", "sup_tok"}

// Status is what the strawboss pane should currently look like in herdr.
type Status struct {
	State   string // "working" or "idle"
	Message string // last supervisor turn error, if any
	Title   string // most recent delegated task
	Tokens  map[string]string
}

// Alert is a pop-up worth raising (a live worker failure).
type Alert struct{ Title, Body string }

// Tracker folds the feed into a Status. It is pure — no I/O — so the tee
// can run it inline on every message.
type Tracker struct {
	turn     bool
	turnErr  string
	title    string
	workers  map[string]string // id → status
	local    map[string]int    // id → fresh input + output tokens
	supTotal int
}

// NewTracker returns an idle tracker.
func NewTracker() *Tracker {
	return &Tracker{workers: map[string]string{}, local: map[string]int{}}
}

// Observe folds one feed message in and returns any alert it raises.
func (t *Tracker) Observe(msg tea.Msg) []Alert {
	switch v := msg.(type) {
	// A turn is live from its first streamed signal. SupUserMsg and
	// SupTextDoneMsg are left out: session history replays through them.
	case ui.SupStatusMsg:
		if v.Status != "" {
			t.startTurn()
		}
	case ui.SupTextDeltaMsg, ui.SupTurnUsageMsg:
		t.startTurn()
	case ui.SupToolMsg:
		t.startTurn()
		if d := v.Delegate; d != nil {
			task := d.Task
			if task == "" {
				task = d.TaskFile
			}
			t.title = truncate(firstLine(task), 60)
		}
	case ui.SupTurnDoneMsg:
		t.turn = false
		t.turnErr = v.Err
	case ui.SessionSwitchedMsg:
		t.turn, t.turnErr, t.title = false, "", ""
	case ui.SupUsageMsg:
		t.supTotal += v.Input + v.Output + v.CacheWrite
	case ui.WorkerUpsertMsg:
		prev, seen := t.workers[v.ID]
		if v.Status != "" {
			t.workers[v.ID] = v.Status
		} else if !seen {
			t.workers[v.ID] = ""
		}
		if v.Status == "failed" && prev != "failed" && !v.Replay {
			return []Alert{{
				Title: "strawboss: " + v.ID + " failed",
				Body:  truncate(firstLine(v.Summary), 120),
			}}
		}
	case ui.WorkerUsageMsg:
		t.local[v.ID] = v.Input + v.Output
	}
	return nil
}

func (t *Tracker) startTurn() {
	t.turn = true
	t.turnErr = ""
}

// Status is the pane's current state. Workers still running keep the pane
// working after the supervisor's turn ends: herdr's "done" should mean
// the whole operation settled, not just the supervisor.
func (t *Tracker) Status() Status {
	var running, queued int
	for _, st := range t.workers {
		switch st {
		case "running":
			running++
		case "queued":
			queued++
		}
	}
	state := "idle"
	if t.turn || running+queued > 0 {
		state = "working"
	}
	var workers []string
	if running > 0 {
		workers = append(workers, fmt.Sprintf("%d running", running))
	}
	if queued > 0 {
		workers = append(workers, fmt.Sprintf("%d queued", queued))
	}
	local := 0
	for _, n := range t.local {
		local += n
	}
	tokens := map[string]string{}
	if len(workers) > 0 {
		tokens["workers"] = strings.Join(workers, " · ")
	}
	if local > 0 {
		tokens["local_tok"] = formatTokens(local) + " local"
	}
	if t.supTotal > 0 {
		tokens["sup_tok"] = formatTokens(t.supTotal) + " sup"
	}
	return Status{State: state, Message: t.turnErr, Title: t.title, Tokens: tokens}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
