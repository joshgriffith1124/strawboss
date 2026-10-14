package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

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
	head := pill("● claude-supervisor ›", cTerm, cSup)
	// Header right, most important first; the model name drops when the
	// pane is too narrow for everything (the sidebar shows it too).
	var parts []string
	if n := len(m.activeWorkers()); m.hideTiles && n > 0 {
		// Tiles hidden with ctrl+t: say how many are tucked away.
		parts = append(parts, sAmber.Render(fmt.Sprintf("%d tiles hidden · ctrl+t", n)))
	}
	if m.rolesOn {
		// The supervisor is being offered roles and pipelines (ctrl+r).
		parts = append(parts, sAmber.Render("roles on"))
	}
	if ctx := m.supCtxLabel(); ctx != "" {
		parts = append(parts, ctx)
	}
	sep := sFaint.Render(" · ")
	right := strings.Join(parts, sep)
	// Repo/branch after the pill, dimmed. It outranks the model name (the
	// sidebar shows that too) but not the indicators: it takes what they
	// leave, dropping the repo name before cutting the branch, and drops
	// entirely when not even a readable branch fits. Leaving one spare
	// cell (room = gap-2) keeps pane()'s gap ≥ 1.
	if room := w - 6 - lipgloss.Width(right) - lipgloss.Width(head); room >= 4 {
		if label := m.repoLabel(room); label != "" {
			head += sDim.Render(" " + label)
		}
	}
	if m.supModel != "" {
		withModel := sDim.Render(m.supModel)
		if right != "" {
			withModel = right + sep + withModel
		}
		if lipgloss.Width(head)+lipgloss.Width(withModel)+4 <= w-2 {
			right = withModel
		}
	}
	return pane(head, right, lines, w, h, border)
}

// repoLabel is the repo and branch the supervisor works in, fitted to room
// cells: "strawboss @ repo-info-pane *" (`*` = uncommitted changes), else
// "@ repo-info-pane *" without the repo name, else the branch cut short.
// Plain ASCII on purpose: terminals that draw "⎇" from a fallback font
// paint it wide and hide the branch's last letter.
// Empty until the first RepoInfoMsg arrives.
func (m Model) repoLabel(room int) string {
	if m.repoName == "" {
		return ""
	}
	branch := "@ " + m.repoBranch
	if m.repoDirty {
		branch += " *"
	}
	if full := m.repoName + " " + branch; lipgloss.Width(full) <= room {
		return full
	}
	return truncPlain(branch, room)
}

// supCtxLabel is the supervisor's context gauge, "ctx 168k/1.0M 17%": red
// once ctxBloated (every call re-reads it all; /new starts fresh), dim
// otherwise. Unknown window shows the footprint alone; nothing yet is "".
func (m Model) supCtxLabel() string {
	if m.supCtx <= 0 {
		return ""
	}
	label := "ctx " + formatTokens(m.supCtx)
	if win := m.ctxWindow(); win > 0 {
		// Nearest whole percent: 168k of 1M is 17%, not a truncated 16.
		label += fmt.Sprintf("/%s %d%%", formatTokens(win), (100*m.supCtx+win/2)/win)
	}
	if m.ctxBloated() {
		return sErr.Render(label + " · /new")
	}
	return sDim.Render(label)
}

// workerCtxLabel is a worker's context gauge, "ctx 24k/131k 18%" — the same
// shape as supCtxLabel, amber from 70% (the DETAIL column's threshold).
// Unknown window shows the footprint alone; nothing yet is "".
func (m Model) workerCtxLabel(wk *workerRow) string {
	if wk.Ctx <= 0 {
		return ""
	}
	label := "ctx " + formatTokens(wk.Ctx)
	if win := m.contextWindowFor(wk.Model); win > 0 {
		pct := (100*wk.Ctx + win/2) / win
		label += fmt.Sprintf("/%s %d%%", formatTokens(win), pct)
		if pct >= 70 {
			return sAmberB.Render(label)
		}
	}
	return sDim.Render(label)
}

// countdownLabel is the time left to a deadline: "⏱ 4m" (minutes, rounded
// up), "⏱ <1m" under a minute, "⏱ 0m" once past. style returns the style
// for the label — amber under five minutes, dim otherwise.
func (m Model) countdownLabel(d time.Time) (string, lipgloss.Style) {
	label, style := m.timeLeft(d)
	return "⏱ " + label, style
}

// timeLeft is the minutes to d by the UI clock, rounded up — "4m", "<1m",
// "0m" once past — styled amber under five minutes, dim otherwise.
func (m Model) timeLeft(d time.Time) (string, lipgloss.Style) {
	left := d.Sub(m.now)
	label := fmt.Sprintf("%dm", int(math.Ceil(left.Minutes())))
	switch {
	case left <= 0:
		label = "0m"
	case left < time.Minute:
		label = "<1m"
	}
	if left < 5*time.Minute {
		return label, sAmberB
	}
	return label, sDim
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

	// Head pill by status; default keeps the queued look before status
	// arrives. A role task names its role after the id ("w12 reviewer ›").
	label := wk.ID
	if wk.Role != "" {
		label += " " + wk.Role
	}
	if wk.Pipeline != "" {
		// Where in the chain: "w906 coder · feature 2/3".
		label += " · " + wk.Pipeline + " " + wk.Step
	}
	text, fg, bg := glyphQueued+" "+label+" ›", cAmber, cKeyBg
	switch wk.Status {
	case "running":
		text, fg, bg = glyphRun+" "+label+" ›", cOK, lipgloss.Color("#0E2417")
	case "done":
		text, fg, bg = glyphDone+" "+label+" ›", cOK, lipgloss.Color("#0E2417")
	case "failed":
		text, fg, bg = glyphFail+" "+label+" ›", cErr, lipgloss.Color("#2A1214")
	}
	head := pill(text, fg, bg)
	// Header right like the supervisor pane: the deadline countdown (when
	// one is known) first, then the context gauge; the model name drops
	// when narrow but the countdown and ctx label stay.
	var right string
	if wk.Status == "running" && !wk.Deadline.IsZero() {
		label, style := m.countdownLabel(wk.Deadline)
		right = style.Render(label)
	}
	if ctx := m.workerCtxLabel(wk); ctx != "" {
		if right != "" {
			right += sFaint.Render(" · ")
		}
		right += ctx
	}
	if model := sDim.Render(wk.Model); right == "" {
		right = model
	} else if withModel := right + sFaint.Render(" · ") + model; lipgloss.Width(head)+lipgloss.Width(withModel)+4 <= w-2 {
		right = withModel
	}
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
	// its first line, continuation lines align with its text. Only the
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
// first line only; continuation lines hang under the first line's text.
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
	hang := " " + strings.Repeat(" ", lipgloss.Width(prefix))
	out := make([]string, 0, len(ws))
	for i, ln := range ws {
		if i == 0 {
			out = append(out, " "+prefix+ln)
		} else {
			out = append(out, hang+ln)
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
