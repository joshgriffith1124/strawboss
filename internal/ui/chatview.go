package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const sidePanelWidth = 38

func (m Model) viewChat(w, h int) string {
	sideW := sidePanelWidth
	chatW := w - sideW - 1
	if chatW < 30 {
		sideW = 0
		chatW = w
	}

	chatCol := m.viewChatColumn(chatW, h)
	if sideW == 0 {
		return chatCol
	}
	side := m.viewSidePanel(sideW)
	return lipgloss.JoinHorizontal(lipgloss.Top, chatCol, " ", side)
}

func (m Model) viewChatColumn(w, h int) string {
	inputH := 3
	logH := h - inputH
	if logH < 3 {
		logH = 3
	}

	var b strings.Builder
	wrap := lipgloss.NewStyle().Width(w - 2)
	for _, it := range m.chat {
		switch it.kind {
		case "user":
			// .Local(): stream timestamps arrive in UTC; wall-clock labels
			// must agree with the user's clock.
			b.WriteString(sRun.Render("YOU · "+it.when.Local().Format("15:04")) + "\n")
			b.WriteString(wrap.Render(sBrite.Render(hardWrap(it.text, w-4))) + "\n\n")
		case "sup":
			b.WriteString(sAmber.Render("SUPERVISOR · "+it.when.Local().Format("15:04")) + "\n")
			b.WriteString(wrap.Render(mdInline(hardWrap(it.text, w-4))) + "\n\n")
		case "tool-out":
			b.WriteString(toolBlock(sAmber.Render(glyphOut), it.text, 240, sText, w))
		case "tool-in":
			mark := sOK.Render(glyphDone)
			if it.isError {
				mark = sErr.Render(glyphFail)
			}
			// Delegation results get room — their content (summaries,
			// refusals) is the point. Every other tool result (Read blobs,
			// edit confirmations) collapses to one dim line: the raw
			// content is supervisor food, not conversation.
			maxLen := 500
			if !isDelegationResult(firstLine(it.text)) && !it.isError {
				maxLen = w - 20
				if maxLen > 160 {
					maxLen = 160
				}
			}
			b.WriteString(toolBlock(sTeal.Render(glyphIn)+" "+mark, it.text, maxLen, sDim, w))
		case "note":
			style := sDim
			if it.isError {
				style = sErr
			}
			b.WriteString("  " + style.Render(truncPlain(it.text, w-4)) + "\n")
		}
	}
	if m.streaming.Len() > 0 {
		b.WriteString(sAmber.Render("SUPERVISOR") + "\n")
		b.WriteString(wrap.Render(mdInline(hardWrap(m.streaming.String(), w-4))) + "\n")
	}
	if m.supStatus != "" {
		star := glyphStream
		if m.pulse {
			star = sAmber.Render(glyphStream)
		} else {
			star = sFaint.Render(glyphStream)
		}
		b.WriteString("  " + star + " " + sDim.Italic(true).Render(m.supStatus) + "\n")
	}

	lines := tail(strings.TrimRight(b.String(), "\n"), logH)
	// Bottom-anchor: the conversation grows up from the input box, so the
	// newest exchange sits beside where you type instead of stranded at
	// the top of an empty column.
	for len(lines) < logH {
		lines = append([]string{""}, lines...)
	}
	log := strings.Join(lines, "\n")

	m.input.Width = w - 6
	inputBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).
		Width(w - 2).Render(m.input.View())

	return lipgloss.JoinVertical(lipgloss.Left, log, inputBox)
}

// toolBlock renders an inline tool line that WRAPS instead of vanishing
// behind an ellipsis: denial reasons and task text must stay readable. cap
// bounds runaway content (giant task prompts) before wrapping. hardWrap
// first: unbroken runs (JSON blobs in tool results) overflow word-wrap
// and shove the side panel off-screen (seen live).
func toolBlock(prefix, text string, maxLen int, style lipgloss.Style, w int) string {
	text = hardWrap(truncPlain(text, maxLen), w-8)
	wrapped := lipgloss.NewStyle().Width(w - 6).Render(style.Render(text))
	lines := strings.Split(wrapped, "\n")
	var b strings.Builder
	b.WriteString("  " + prefix + " " + lines[0] + "\n")
	for _, ln := range lines[1:] {
		b.WriteString("      " + ln + "\n")
	}
	return b.String()
}

// hardWrap chops any unbroken run longer than w into w-sized pieces so
// the word-wrapper can always break lines (it never splits words itself).
// Plain text only — apply before styling.
func hardWrap(s string, w int) string {
	if w < 8 {
		return s
	}
	var b strings.Builder
	for i, word := range strings.Split(s, " ") {
		if i > 0 {
			b.WriteByte(' ')
		}
		runes := []rune(word)
		for len(runes) > w {
			b.WriteString(string(runes[:w]))
			b.WriteByte(' ')
			runes = runes[w:]
		}
		b.WriteString(string(runes))
	}
	return b.String()
}

// mdInline styles the markdown the supervisor actually writes — **bold**
// and `code` — instead of showing asterisk soup. Line-scoped so styling
// never spans a wrap boundary; unpaired markers pass through untouched.
func mdInline(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = mdStylePairs(line, "**", sBoldT)
		line = mdStylePairs(line, "`", sTeal)
		out = append(out, sText.Render(line))
	}
	return strings.Join(out, "\n")
}

// mdStylePairs replaces marker-delimited spans with styled text, only
// when markers pair up.
func mdStylePairs(line, marker string, style lipgloss.Style) string {
	parts := strings.Split(line, marker)
	if len(parts) < 3 {
		return line
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 && i < len(parts) {
			if i == len(parts)-1 { // trailing unpaired marker
				b.WriteString(marker + p)
				continue
			}
			b.WriteString(style.Render(p))
			continue
		}
		b.WriteString(p)
	}
	return b.String()
}

func (m Model) viewSidePanel(w int) string {
	panels := []string{
		m.viewTokensPanel(w),
		m.viewWorkersMini(w),
		m.viewModelsPanel(w),
	}
	return lipgloss.JoinVertical(lipgloss.Left, panels...)
}

func kv(w int, k, v string) string {
	gap := w - 2 - lipgloss.Width(k) - lipgloss.Width(v) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + sDim.Render(k) + strings.Repeat(" ", gap) + v + " "
}

func (m Model) viewTokensPanel(w int) string {
	wrkIn, wrkOut := 0, 0
	for _, wk := range m.workers {
		wrkIn += wk.In
		wrkOut += wk.Out
	}
	wrkTotal := wrkIn + wrkOut

	// Headline numbers are FRESH tokens: supervisor cache reads are the
	// conversation prefix re-read every turn — folding them into the
	// headline made the paid side look enormous. They get their own dim
	// line instead.
	freshSup := m.supIn + m.supCacheWrite + m.supOut
	lines := []string{
		kv(w, "supervisor", sText.Render(formatTokens(freshSup)+" · ")+sAmberB.Render("plan")),
	}
	if m.supCacheRead > 0 {
		lines = append(lines, kv(w, "  cache reads", sFaint.Render(formatTokens(m.supCacheRead)+" · free-ish")))
	}
	lines = append(lines,
		kv(w, "workers", sText.Render(formatTokens(wrkTotal)+" · ")+sTealB.Render("$0.00")),
	)
	barW := w - 4
	if barW > 4 && freshSup+wrkTotal > 0 {
		supCells := barW * freshSup / (freshSup + wrkTotal)
		if supCells < 1 && freshSup > 0 {
			supCells = 1
		}
		bar := sAmber.Render(strings.Repeat("▰", supCells)) + sTeal.Render(strings.Repeat("▰", barW-supCells))
		lines = append(lines, " "+bar+" ")
		supPct := 100 * freshSup / (freshSup + wrkTotal)
		legend := sDim.Render("fresh ") + sAmberB.Render(fmt.Sprintf("%d%%", supPct)) + sDim.Render(" plan")
		right := sTealB.Render(fmt.Sprintf("%d%%", 100-supPct)) + sDim.Render(" local")
		gap := w - 2 - lipgloss.Width(legend) - lipgloss.Width(right) - 2
		if gap < 1 {
			gap = 1
		}
		lines = append(lines, " "+legend+strings.Repeat(" ", gap)+right+" ")
	}
	if m.fiveHour > 0 {
		lines = append(lines, kv(w, "plan window",
			sText.Render(fmt.Sprintf("5h %.0f%% · 7d %.0f%%", m.fiveHour*100, m.sevenDay*100))))
	}
	return panel("Tokens · plan vs free local", lines, w, cSupBorder, cAmber)
}

func (m Model) viewWorkersMini(w int) string {
	active := 0
	for _, wk := range m.workers {
		if wk.Status == "running" {
			active++
		}
	}
	var lines []string
	rows := m.sortedWorkers()
	max := 6
	for _, wk := range rows {
		if max == 0 {
			break
		}
		max--
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
		mdl := truncPlain(wk.Model, 12)
		// No task fragment here — a five-char "Write…" says nothing; the
		// dashboard owns task text. Failed rows keep their error, the one
		// label worth the width.
		label := ""
		if wk.Status == "failed" {
			label = sErr.Render(truncPlain(firstLine(wk.Summary), w-26-len(mdl)))
		}
		left := strings.TrimRight(fmt.Sprintf(" %-3s %s %s %s", wk.ID, statusGlyph(wk.Status, m.pulse),
			sTeal.Render(mdl), label), " ")
		right := sDim.Render(fmt.Sprintf("%5s %6s ", dur, toks))
		gap := w - 2 - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
		lines = append(lines, left+strings.Repeat(" ", gap)+right)
	}
	if len(lines) == 0 {
		lines = []string{" " + sFaint.Render("no workers yet")}
	}
	return panel(fmt.Sprintf("Workers · %d active", active), lines, w, cWrkBorder, cTeal)
}

func (m Model) viewModelsPanel(w int) string {
	// Rows only for models with something to say — active, generating,
	// unreachable, or configured-but-not-served. A stack of "idle" lines
	// reads as noise, so healthy idle models collapse to one count.
	var lines []string
	idle := 0
	for _, ms := range m.models {
		val := sFaint.Render("idle")
		switch {
		case ms.Note == "endpoint unreachable":
			val = sErr.Render("unreachable")
		case ms.Note == "model not loaded":
			val = sAmber.Render("not loaded")
		case ms.Active > 0 || ms.TokSec > 0:
			if ms.TokSec > 0 {
				val = sText.Render(fmt.Sprintf("%.0f tok/s", ms.TokSec))
			} else {
				val = sText.Render("active")
			}
			if ms.Active > 0 {
				val = sTealB.Render(fmt.Sprintf("%d▶ ", ms.Active)) + val
			}
		default:
			idle++
			continue
		}
		lines = append(lines, kv(w, truncPlain(ms.Name, w-16), val))
	}
	if idle > 0 {
		label := fmt.Sprintf("+%d idle", idle)
		if idle == len(m.models) {
			label = fmt.Sprintf("%d configured · all idle", idle)
		}
		lines = append(lines, kv(w, "models", sFaint.Render(label)))
	}
	// task tally
	var done, running, queued, failed int
	for _, wk := range m.workers {
		switch wk.Status {
		case "done":
			done++
		case "running":
			running++
		case "queued":
			queued++
		case "failed":
			failed++
		}
	}
	lines = append(lines, kv(w, "tasks", taskTally(done, running, queued, failed)))
	return panel("Models", lines, w, cBord, cDim)
}

// taskTally renders the done/running/queued/failed counts with breathing
// room; zero counts fade out rather than clutter.
func taskTally(done, running, queued, failed int) string {
	part := func(style lipgloss.Style, n int, glyph string) string {
		s := fmt.Sprintf("%d%s", n, glyph)
		if n == 0 {
			return sFaint.Render(s)
		}
		return style.Render(s)
	}
	return part(sOK, done, glyphDone) + sFaint.Render(" · ") +
		part(sRun, running, glyphRun) + sFaint.Render(" · ") +
		part(sDim, queued, glyphQueued) + sFaint.Render(" · ") +
		part(sErr, failed, glyphFail)
}
