package ui

import (
	"fmt"
	"strconv"
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
	case m.tab == tabWorktrees && m.OnListBranches == nil:
		// Demo replay: the tab is up but no branch hook is wired, so the
		// review has nothing to list — a faint notice instead.
		body = m.viewWorktreesDemo(w, contentH)
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
	case m.tab == tabModels:
		body = m.viewModelsTab(w, contentH)
	case m.tab == tabAnalytics:
		body = m.viewAnalyticsTab(w, contentH)
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

// viewTopTabs is the merged top row: logo left, the six numbered tabs in
// the middle, run telemetry right. When the row is too narrow the
// right-hand status drops first, then the labels shorten to their first
// three letters; either way the row comes back exactly w cells.
func (m Model) viewTopTabs(w int) string {
	left := sBoldT.Render(glyphLogo + " strawboss")
	right := m.viewTopRight()
	full, short := m.viewTabCells(false), m.viewTabCells(true)

	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	fw, sw := lipgloss.Width(full), lipgloss.Width(short)
	switch {
	case w >= lw+fw+rw+4:
		return m.topRow(w, left, full, right)
	case w >= lw+sw+rw+4:
		return m.topRow(w, left, short, right)
	case w >= lw+fw+3:
		return m.topRow(w, left, full, "")
	default:
		return m.topRow(w, left, short, "")
	}
}

// topRow lays the top-row parts across exactly w cells: left at the left
// edge, right right-aligned with a two-cell margin (when present), the
// leftover space sitting between left and mid.
func (m Model) topRow(w int, left, mid, right string) string {
	if right == "" {
		gap := w - lipgloss.Width(left) - lipgloss.Width(mid)
		if gap < 1 {
			return padTo(left+mid, w)
		}
		return left + strings.Repeat(" ", gap) + mid
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(mid) - lipgloss.Width(right) - 2
	if gap < 2 {
		return padTo(left+" "+mid+" "+right, w)
	}
	return left + strings.Repeat(" ", gap/2+gap%2) + mid + strings.Repeat(" ", gap/2) + right + "  "
}

// viewTabCells renders all six tabs from tabNames ("1 Agents  2 Tasks …");
// the active tab is the sTabOn pill (1-cell padding), numbers stay faint
// in every tab. short cuts each label to its first three letters — the
// narrow-row fallback.
func (m Model) viewTabCells(short bool) string {
	var cells []string
	for i, name := range tabNames {
		if short && len(name) > 3 {
			name = name[:3]
		}
		n := sFaint.Render(strconv.Itoa(i + 1))
		if i == m.tab {
			cells = append(cells, sTabOn.Render(" "+n+" "+name+" "))
		} else {
			cells = append(cells, sTabOff.Render(" "+n+" "+name+" "))
		}
	}
	return strings.Join(cells, " ")
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
			tokPerSec += m.liveRate(wk.ID)
		case "queued":
			agents++
		}
	}
	rightParts = append(rightParts, sOK.Render("●")+" "+
		sDim.Render(fmt.Sprintf("%d models up · %d agents · %.0f tok/s", modelsUp, agents, tokPerSec)))
	return strings.Join(rightParts, sFaint.Render(" · "))
}

// viewWorktreesDemo is the Worktrees tab with no branch hook wired
// (demo replay): a faint notice, in a pane that still fills w×h.
func (m Model) viewWorktreesDemo(w, h int) string {
	return pane(sDim.Render("WORKTREES · strawboss/* branches"), "",
		[]string{" " + sFaint.Render("demo replay — no branches")}, w, h, cSupBorder)
}

// viewFooter is the status bar: worker status left, key hints, clock right.
func (m Model) viewFooter(w int) string {
	var hints [][2]string
	switch {
	case m.reviewing && m.diffText != "":
		hints = [][2]string{{"↑↓/PgUp/Dn", "scroll"}, {"esc", "back"}, {"⇥/1-6", "tabs"}}
	case m.reviewing:
		hints = [][2]string{{"↑↓", "select"}, {"↵", "diff"}, {"m", "merge"}, {"x", "discard"}, {"r", "refresh"}, {"esc", "close"}, {"⇥/1-6", "tabs"}}
	case m.picking:
		hints = [][2]string{{"↑↓", "select"}, {"↵", "switch"}, {"n", "new"}, {"esc", "close"}}
	case m.tab == tabChat:
		if m.focus == focusBottom {
			hints = [][2]string{{"⇧⇥", "pane"}, {"⇥", "tabs"}, {"←→", "panel"}, {"↑↓", "select"}, {"x", "kill"}, {"+", "extend"}, {"r", "retry"}, {"u", "undo"}, {"f", "log filter"}, {"z", "zoom"}, {"esc", "back"}, {"q", "quit"}}
		} else if m.focus == focusWork {
			hints = [][2]string{{"⇧⇥", "pane"}, {"⇥", "tabs"}, {"z", "zoom"}, {"↑↓", "select"}, {"x", "kill"}, {"+", "extend"}, {"r", "retry"}, {"u", "undo"}, {"t", "panel"}, {"b", "branches"}, {"q", "quit"}}
		} else {
			hints = [][2]string{{"↵", "send"}, {"alt+v", "image"}, {"⇧⇥", "pane"}, {"⇥", "tabs"}, {"ctrl+o", "zoom"}, {"ctrl+t", "tiles"}, {"ctrl+r", "roles"}, {"PgUp/Dn", "scroll"}, {"esc", "interrupt"}, {"ctrl+c", "quit"}}
		}
	case m.tab == tabModels || m.tab == tabAnalytics:
		hints = [][2]string{{"⇥/1-6", "tabs"}, {"shift+tab", "prev"}, {"q", "quit"}}
	case m.tab == tabDashboard:
		hints = [][2]string{{"↑↓", "select"}, {"/", "filter"}, {"s", "sessions"}, {"b", "branches"}, {"n", "new"}, {"x", "kill"}, {"+", "extend"}, {"r", "retry"}, {"R", "retry-failed"}, {"u", "undo"}, {"⇥/1-6", "tabs"}, {"q", "quit"}}
	default:
		hints = [][2]string{{"f", "filter"}, {"s", "sessions"}, {"n", "new"}, {"⇥/1-6", "tabs"}, {"q", "quit"}}
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
	// One leading space plus left + gap + clock must land exactly on w.
	gap := w - 1 - lipgloss.Width(left) - lipgloss.Width(right)
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
