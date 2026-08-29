package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewSessionPicker lists THIS project's past supervisor sessions:
// newest first, labeled by first prompt, with each run's worker tally.
func (m Model) viewSessionPicker(w, h int) string {
	inner := h - 2
	if inner < 1 {
		inner = 1
	}
	var lines []string
	if len(m.sessions) == 0 {
		lines = []string{" " + sFaint.Render("no session history for this project yet")}
	}
	start := 0
	if m.sessIdx >= inner-1 {
		start = m.sessIdx - inner + 2
	}
	for i := start; i < len(m.sessions) && len(lines) < inner; i++ {
		s := m.sessions[i]
		cursor := " "
		if i == m.sessIdx {
			cursor = sTealB.Render("▸")
		}
		label := s.Label
		if label == "" {
			label = "(no prompt recorded)"
		}
		age := m.now.Sub(s.Started).Round(60e9)
		left := fmt.Sprintf("%s %s  %s", cursor,
			sDim.Render(s.Started.Local().Format("Jan 02 15:04")),
			sText.Render(truncPlain(label, w-52)))
		right := sDim.Render(fmt.Sprintf("%dw ", s.Workers)) +
			sOK.Render(fmt.Sprintf("%d%s ", s.Done, glyphDone)) +
			sErr.Render(fmt.Sprintf("%d%s", s.Failed, glyphFail)) +
			sFaint.Render(fmt.Sprintf("  %6s ago ", age))
		if s.Current {
			right = sTealB.Render("current ") + right
		}
		gap := w - 4 - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
		row := left + strings.Repeat(" ", gap) + right
		if i == m.sessIdx {
			row = sSelRow.Render(padTo(row, w-2))
		}
		lines = append(lines, row)
	}
	return panel("Sessions · this project", padLines(lines, inner), w, cSupBorder, cAmber)
}
