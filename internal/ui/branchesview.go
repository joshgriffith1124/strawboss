package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewBranchPicker lists this project's strawboss/* worker branches
// awaiting review: two lines each (name + diff stats, then the subject),
// selected entry highlighted. The pane fills w×h.
func (m Model) viewBranchPicker(w, h int) string {
	inner := h - 3 // the pane's content rows (header + top/bottom border)
	if inner < 0 {
		inner = 0
	}
	var lines []string
	if len(m.branches) == 0 {
		lines = []string{" " + sFaint.Render("no strawboss/* branches — delegate --worktree leaves work here")}
	} else {
		// Each entry occupies two lines; scroll so the selected entry
		// stays fully visible.
		start := m.brIdx - inner/2 + 1
		if start < 0 {
			start = 0
		}
		for i := start; i < len(m.branches) && len(lines) < inner; i++ {
			b := m.branches[i]
			cursor := " "
			if i == m.brIdx {
				cursor = sTealB.Render("▸")
			}
			name := strings.TrimPrefix(b.Name, "strawboss/")
			age := m.now.Sub(b.When).Round(60e9)
			right := sDim.Render(fmt.Sprintf("%dc ", b.Ahead)) +
				sDim.Render(fmt.Sprintf("%df ", b.Files)) +
				sOK.Render(fmt.Sprintf("+%d ", b.Added)) +
				sErr.Render(fmt.Sprintf("-%d ", b.Deleted)) +
				sFaint.Render(fmt.Sprintf("%6s ago", age))
			left := cursor + " " + sText.Render(truncPlain(name, w-4-lipgloss.Width(right)))
			gap := w - 4 - lipgloss.Width(left) - lipgloss.Width(right)
			if gap < 1 {
				gap = 1
			}
			row := left + strings.Repeat(" ", gap) + right
			subj := "    " + sFaint.Render(truncPlain(b.Subject, w-8))
			if i == m.brIdx {
				row = sSelRow.Render(padTo(row, w-2))
				subj = sSelRow.Render(padTo(subj, w-2))
			}
			lines = append(lines, row, subj)
		}
	}
	return pane(sDim.Render("WORKTREES · strawboss/* branches"), "", lines, w, h, cSupBorder)
}

// viewBranchDiff shows the selected branch's diff from diffScroll,
// git-diff colored: context plain, additions green, deletions red,
// hunk headers and file headers in the worker teal. The pane fills
// w×h, the branch name in the head.
func (m Model) viewBranchDiff(w, h int) string {
	inner := h - 3 // the pane's content rows (header + top/bottom border)
	if inner < 0 {
		inner = 0
	}
	lines := strings.Split(m.diffText, "\n")
	start := m.diffScroll
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	var out []string
	for i := start; i < len(lines) && len(out) < inner; i++ {
		ln := truncPlain(lines[i], w-4)
		style := sText
		switch {
		case strings.HasPrefix(lines[i], "+++"), strings.HasPrefix(lines[i], "---"):
			style = sDim
		case strings.HasPrefix(lines[i], "+"):
			style = sOK
		case strings.HasPrefix(lines[i], "-"):
			style = sErr
		case strings.HasPrefix(lines[i], "@@"):
			style = sTeal
		case strings.HasPrefix(lines[i], "diff --git"):
			style = sTealB
		}
		out = append(out, style.Render(ln))
	}
	name := ""
	if m.brIdx < len(m.branches) {
		name = strings.TrimPrefix(m.branches[m.brIdx].Name, "strawboss/")
	}
	head := "DIFF"
	if name != "" {
		head = "DIFF · " + name
	}
	return pane(sDim.Render(head), "", out, w, h, cSupBorder)
}
