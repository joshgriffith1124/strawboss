package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w := m.width
	if w <= 0 {
		w = 100
	}
	h := m.height
	if h <= 0 {
		h = 30
	}

	top := m.viewTopTabs(w)
	foot := m.viewFooter(w)
	contentH := h - 2 // merged top row + footer
	if m.toast != "" {
		contentH--
	}
	if contentH < 1 {
		contentH = 1 // degenerate sizes: a one-line body beats a crash
	}

	var body string
	switch {
	case m.reviewing && m.diffText != "":
		body = m.viewBranchDiff(w, contentH)
	case m.reviewing:
		body = m.viewBranchPicker(w, contentH)
	case m.picking:
		body = m.viewSessionPicker(w, contentH)
	case m.tab == tabChat:
		body = m.viewAgents(w, contentH)
	case m.tab == tabDashboard:
		body = m.viewDashboard(w, contentH)
	case m.tab == tabLogs:
		body = m.viewLogs(w, contentH)
	}

	parts := []string{top}
	if m.toast != "" {
		toast := sErr.Render(m.toast)
		parts = append(parts, padLeftTo(toast, w-2))
	}
	parts = append(parts, body, foot)
	return strings.Join(parts, "\n")
}

// viewTopTabs is the merged top row: logo left, numbered tabs in the
// middle, run telemetry right. The old top bar's session/pid text is gone
// — the Agents screen owns the supervisor's identity.
func (m Model) viewTopTabs(w int) string {
	left := sBoldT.Render(glyphLogo + " strawboss")
	mid := m.viewTabCells()
	right := m.viewTopRight()

	gap := w - lipgloss.Width(left) - lipgloss.Width(mid) - lipgloss.Width(right) - 2
	if gap < 2 {
		return padTo(left+" "+mid+" "+right, w)
	}
	return left + strings.Repeat(" ", gap/2+gap%2) + mid + strings.Repeat(" ", gap/2) + right + "  "
}

// viewTabCells renders "1 Agents  2 Tasks  3 Logs"; the active tab is the
// sTabOn pill (1-cell padding), numbers are faint in every tab.
func (m Model) viewTabCells() string {
	names := []string{"Agents", "Tasks", "Logs"}
	var cells []string
	for i, name := range names {
		n := sFaint.Render(fmt.Sprintf("%d", i+1))
		if i == m.tab {
			cells = append(cells, sTabOn.Render(" "+n+" "+name+" "))
		} else {
			cells = append(cells, sTabOff.Render(" "+n+" "+name+" "))
		}
	}
	return strings.Join(cells, "  ")
}

// viewTopRight is the right side of the merged top row: the remote
// channel (when armed), the 5h plan-window gauge, and the mockup's fleet
// readout "● N models up · N agents · N tok/s" — agents counts the
// supervisor plus the live (running/queued) workers; the old run clock
// and raw running count are gone.
func (m Model) viewTopRight() string {
	var rightParts []string
	if m.remoteChannel != "" {
		rightParts = append(rightParts, sTeal.Render("⇄ "+m.remoteChannel))
	}
	if m.fiveHour > 0 {
		gauge := sDim.Render(fmt.Sprintf("5h %.0f%%", m.fiveHour*100))
		if m.fiveHour >= 0.7 {
			gauge = sAmberB.Render(fmt.Sprintf("5h %.0f%%", m.fiveHour*100))
		}
		rightParts = append(rightParts, gauge)
	}
	modelsUp := len(m.models)
	if modelsUp == 0 && m.modelSet != nil {
		modelsUp = len(m.modelSet)
	}
	agents, tokPerSec := 1, 0.0
	for _, wk := range m.workers {
		switch wk.Status {
		case "running":
			agents++
			tokPerSec += m.workerRates[wk.ID].rate
		case "queued":
			agents++
		}
	}
	rightParts = append(rightParts, sOK.Render("●")+" "+
		sDim.Render(fmt.Sprintf("%d models up · %d agents · %.0f tok/s", modelsUp, agents, tokPerSec)))
	return strings.Join(rightParts, sFaint.Render(" · "))
}

// viewFooter is the status bar: worker status left, key hints, clock right.
func (m Model) viewFooter(w int) string {
	var hints [][2]string
	switch {
	case m.reviewing && m.diffText != "":
		hints = [][2]string{{"↑↓/PgUp/Dn", "scroll"}, {"esc", "back"}}
	case m.reviewing:
		hints = [][2]string{{"↑↓", "select"}, {"↵", "diff"}, {"m", "merge"}, {"x", "discard"}, {"r", "refresh"}, {"esc", "close"}}
	case m.picking:
		hints = [][2]string{{"↑↓", "select"}, {"↵", "switch"}, {"n", "new"}, {"esc", "close"}}
	case m.tab == tabChat:
		if m.focus == focusWork {
			hints = [][2]string{{"⇥", "pane"}, {"z", "zoom"}, {"↑↓", "select"}, {"x", "kill"}, {"r", "retry"}, {"u", "undo"}, {"t", "panel"}, {"b", "branches"}, {"1-3", "tabs"}, {"q", "quit"}}
		} else {
			hints = [][2]string{{"↵", "send"}, {"alt+v", "image"}, {"⇥", "pane"}, {"alt+z", "zoom"}, {"PgUp/Dn", "scroll"}, {"esc", "interrupt"}, {"ctrl+c", "quit"}}
		}
	case m.tab == tabDashboard:
		hints = [][2]string{{"↑↓", "select"}, {"/", "filter"}, {"s", "sessions"}, {"b", "branches"}, {"n", "new"}, {"x", "kill"}, {"r", "retry"}, {"R", "retry-failed"}, {"u", "undo"}, {"⇥/1-3", "tabs"}, {"q", "quit"}}
	default:
		hints = [][2]string{{"f", "filter"}, {"s", "sessions"}, {"n", "new"}, {"⇥/1-3", "tabs"}, {"q", "quit"}}
	}
	var parts []string
	failed := 0
	for _, wk := range m.workers {
		if wk.Status == "failed" {
			failed++
		}
	}
	if failed > 0 {
		parts = append(parts, sErr.Render(fmt.Sprintf("%d failed", failed)))
	} else {
		parts = append(parts, sOK.Render("● ok"))
	}
	for _, h := range hints {
		parts = append(parts, sKeycap.Render(h[0])+" "+sDim.Render(h[1]))
	}
	left := strings.Join(parts, "  ")
	right := sDim.Render(m.now.Local().Format("15:04:05"))
	gap := w - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		return padTo(left, w)
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

func padLeftTo(s string, w int) string {
	gap := w - lipgloss.Width(s)
	if gap < 0 {
		gap = 0
	}
	return strings.Repeat(" ", gap) + s
}
