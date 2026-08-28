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

	top := m.viewTopbar(w)
	tabs := m.viewTabs(w)
	foot := m.viewFooter(w)
	contentH := h - 3 // topbar + tabs + footer
	if m.toast != "" {
		contentH--
	}

	var body string
	switch m.tab {
	case tabChat:
		body = m.viewChat(w, contentH)
	case tabDashboard:
		body = m.viewDashboard(w, contentH)
	case tabLogs:
		body = m.viewLogs(w, contentH)
	}

	parts := []string{top, tabs}
	if m.toast != "" {
		toast := sErr.Render(m.toast)
		parts = append(parts, padLeftTo(toast, w-2))
	}
	parts = append(parts, body, foot)
	return strings.Join(parts, "\n")
}

func (m Model) viewTopbar(w int) string {
	left := sTeal.Render(glyphLogo+" ") + sBoldT.Render("strawboss")
	mid := ""
	if m.sessionID != "" {
		sid := m.sessionID
		if len(sid) > 8 {
			sid = sid[:8] + "…"
		}
		pid := ""
		if m.pid > 0 {
			pid = fmt.Sprintf("supervisor pid %d · ", m.pid)
		}
		mid = sDim.Render(fmt.Sprintf("%s%s · %s auth · session %s", pid, m.supModel, m.auth, sid))
	} else {
		mid = sDim.Render(m.auth)
	}
	right := sDim.Render("run " + formatClock(m.now.Sub(m.started)))

	gap1 := w - lipgloss.Width(left) - lipgloss.Width(mid) - lipgloss.Width(right) - 2
	if gap1 < 2 {
		return padTo(left+" "+mid, w)
	}
	return left + strings.Repeat(" ", gap1/2+gap1%2) + mid + strings.Repeat(" ", gap1/2) + right + "  "
}

func (m Model) viewTabs(w int) string {
	names := []string{"chat", "dashboard", "logs"}
	var cells []string
	for i, name := range names {
		n := fmt.Sprintf("%d", i+1)
		if i == m.tab {
			cells = append(cells, sTabOn.Render(" "+sTeal.Render(n)+" "+name+" "))
		} else {
			cells = append(cells, sTabOff.Render(" "+sFaint.Render(n)+" "+name+" "))
		}
	}
	row := strings.Join(cells, sFaint.Render("│"))
	return padTo(row, w-1) + " "
}

func (m Model) viewFooter(w int) string {
	var hints [][2]string
	switch m.tab {
	case tabChat:
		hints = [][2]string{{"↵", "send"}, {"⇥", "tabs"}, {"esc", "interrupt"}, {"ctrl+c", "quit"}}
	case tabDashboard:
		hints = [][2]string{{"↑↓", "select"}, {"f", "follow"}, {"⇥/1-3", "tabs"}, {"q", "quit"}}
	default:
		hints = [][2]string{{"⇥/1-3", "tabs"}, {"q", "quit"}}
	}
	var parts []string
	for _, h := range hints {
		parts = append(parts, sKeycap.Render(h[0])+" "+sDim.Render(h[1]))
	}
	left := strings.Join(parts, "  ")
	right := sAmber.Render(glyphBell + " bell on fail")
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

// tail returns the last n lines of a rendered block.
func tail(s string, n int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
