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
// the head, the task's first line, the transcript tail (wrapped, newest at
// the bottom), and for finished workers the summary with duration and tokens.
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
	// Transcript: each event's text is wrapped to the pane's inner width
	// instead of cut to one line — the event's colour and glyph stay on
	// its first line, continuation lines drop two cells in. Only the
	// wrapped lines that fit are kept, newest at the bottom as before.
	evs := m.workerEvents[wk.ID]
	var wrapped []string
	for i, ev := range evs {
		switch ev.kind {
		case "tool":
			mark := sOK.Render(glyphDone)
			if wk.Status == "running" && i == len(evs)-1 {
				mark = sRun.Render(glyphRun)
			}
			// " " margin + "✓ " prefix: three cells ahead of the text.
			wrapped = append(wrapped, wrapEventLines(mark+" ", sDim, ev.text, inner-3)...)
		case "error":
			wrapped = append(wrapped, wrapEventLines(sErr.Render(glyphFail)+" ", sErr, ev.text, inner-3)...)
		case "reasoning":
			wrapped = append(wrapped, wrapEventLines("", sFaint, ev.text, inner-2)...)
		default:
			wrapped = append(wrapped, wrapEventLines("", sDim, ev.text, inner-2)...)
		}
	}
	if maxEv > 0 && len(wrapped) > maxEv {
		wrapped = wrapped[len(wrapped)-maxEv:]
	}
	lines = append(lines, wrapped...)
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

// wrapEventLines wraps one worker event's text so every line fits textW
// cells: hardWrap chops runs longer than textW, then lipgloss word-wraps
// the rest (each line comes back padded to exactly textW). The prefix —
// the event's glyph and a space, when the kind has one — sits on the
// first line only; continuation lines are indented two cells.
func wrapEventLines(prefix string, style lipgloss.Style, text string, textW int) []string {
	if textW < 1 {
		textW = 1
	}
	clean := cleanEventText(text)
	block := lipgloss.NewStyle().Width(textW).Render(style.Render(hardWrap(clean, textW)))
	ws := strings.Split(block, "\n")
	// A trailing empty wrap line adds nothing but a stray indent.
	for len(ws) > 0 && ws[len(ws)-1] == "" {
		ws = ws[:len(ws)-1]
	}
	if len(ws) == 0 {
		ws = []string{""}
	}
	out := make([]string, 0, len(ws))
	for i, ln := range ws {
		if i == 0 {
			out = append(out, " "+prefix+ln)
		} else {
			out = append(out, "  "+ln)
		}
	}
	return out
}

// cleanEventText strips ANSI escape sequences and control characters from
// worker output before wrapping: a stray escape mid-line would shred the
// layout (the same sanitisation truncPlain applies, without the cut).
func cleanEventText(s string) string {
	if strings.ContainsRune(s, 0x1b) {
		s = ansiSeq.ReplaceAllString(s, "")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}
