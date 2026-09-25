package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewSupPane is the Agents screen's supervisor pane: the live chat column in
// a rounded box. The input box lives at the column's bottom already.
func (m Model) viewSupPane(w, h int, focused bool) string {
	border := cBord
	if focused {
		border = cFocus
	}
	var lines []string
	for _, ln := range strings.Split(m.viewChatColumn(w-3, h-3), "\n") {
		lines = append(lines, " "+ln)
	}
	return pane(pill("● claude-supervisor ›", cTerm, cSup), sDim.Render(m.supModel), lines, w, h, border)
}

// viewWorkerPane is one worker's detail pane: the status pill plus worktree in
// the head, the task's first line, the transcript tail, and for finished
// workers the summary with duration and tokens.
func (m Model) viewWorkerPane(wk *workerRow, w, h int, focused bool) string {
	border := cBord
	if focused {
		border = cFocus
	}
	if wk == nil {
		// Same one-cell left margin as the lines below.
		return pane(sDim.Render("no worker selected"), "", []string{" " + sFaint.Render("⇥ then ↑↓ to pick a worker")}, w, h, border)
	}

	// Head pill by status; default keeps the queued look before status arrives.
	text, fg, bg := glyphQueued+" "+wk.ID+" ›", cAmber, cKeyBg
	switch wk.Status {
	case "running":
		text, fg, bg = glyphRun+" "+wk.ID+" ›", cOK, lipgloss.Color("#0E2417")
	case "done":
		text, fg, bg = glyphDone+" "+wk.ID+" ›", cOK, lipgloss.Color("#0E2417")
	case "failed":
		text, fg, bg = glyphFail+" "+wk.ID+" ›", cErr, lipgloss.Color("#2A1214")
	}
	right := sDim.Render(wk.Model)
	head := pill(text, fg, bg)
	room := w - 4 - lipgloss.Width(right) - lipgloss.Width(head) - 1
	if room >= 1 {
		head += sDim.Render(" " + truncPlain(wk.Dir, room))
	}

	inner := w - 2
	// One-cell left margin, like viewSupPane: content is budgeted to
	// inner-1 so the prefix never pushes a line past the right border.
	marg := inner - 1
	lines := []string{" " + sDim.Render("> "+truncPlain(firstLine(wk.Task), marg-2))}
	finished := wk.Status == "done" || wk.Status == "failed"
	maxEv := h - 4
	if finished {
		maxEv--
	}
	if maxEv < 0 {
		maxEv = 0
	}
	evs := m.workerEvents[wk.ID]
	if len(evs) > maxEv {
		evs = evs[len(evs)-maxEv:]
	}
	for i, ev := range evs {
		switch ev.kind {
		case "tool":
			mark := sOK.Render(glyphDone)
			if wk.Status == "running" && i == len(evs)-1 {
				mark = sRun.Render(glyphRun)
			}
			lines = append(lines, " "+mark+" "+sDim.Render(truncPlain(ev.text, marg-2)))
		case "error":
			lines = append(lines, " "+sErr.Render(glyphFail+" "+truncPlain(ev.text, marg-2)))
		case "reasoning":
			lines = append(lines, " "+sFaint.Render(truncPlain(ev.text, marg)))
		default:
			lines = append(lines, " "+sDim.Render(truncPlain(ev.text, marg)))
		}
	}
	if finished {
		// Same duration/token rule as viewWorkerTable.
		dur, toks := "—", "—"
		if !wk.Started.IsZero() && wk.Status != "queued" {
			end := m.now
			if !wk.Ended.IsZero() {
				end = wk.Ended
			}
			dur = formatMinSec(end.Sub(wk.Started))
		}
		if wk.In+wk.Out > 0 {
			toks = formatTokens(wk.In + wk.Out)
		}
		style := sDim
		if wk.Status == "failed" {
			style = sErr
		}
		suffix := sDim.Render(" · " + dur + " · " + toks)
		line := style.Render(truncPlain(firstLine(wk.Summary), marg-lipgloss.Width(suffix)))
		if line == "" {
			line = " "
		}
		lines = append(lines, " "+line+suffix)
	}
	return pane(head, right, lines, w, h, border)
}
