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
	if m.streaming != "" {
		b.WriteString(sAmber.Render("SUPERVISOR") + "\n")
		b.WriteString(wrap.Render(mdInline(hardWrap(m.streaming, w-4))) + "\n")
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

	// Scroll window: chatScroll lines up from the bottom (0 = following).
	// Long replies overflow the pane; PgUp is the way back to their start.
	// While scrolled, the bottom row becomes a "N lines below" indicator,
	// so the visible window is one row shorter.
	all := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	scroll, window := m.chatScroll, logH
	if scroll > 0 && logH > 1 {
		window = logH - 1
	}
	if max := len(all) - window; scroll > max {
		scroll = max
	}
	if scroll < 0 {
		scroll = 0
	}
	end := len(all) - scroll
	start := end - window
	if start < 0 {
		start = 0
	}
	lines := append([]string(nil), all[start:end]...)
	if scroll > 0 {
		lines = append(lines, sFaint.Render(fmt.Sprintf("▼ %d lines below · PgDn to follow", scroll)))
	}
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
// and would shove the side panel off-screen.
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

// barCells splits the token bar, and is the only clamp between a token
// count and strings.Repeat — which panics on a negative count and would
// take the whole TUI down, since a render happens outside any recover.
// The ratio is trusted nowhere: a negative or overlarge share (a counter
// that went backwards, a replayed total) clamps into the bar instead.
func barCells(barW, supEquiv, wrkFresh int) int {
	total := supEquiv + wrkFresh
	if barW < 0 {
		return 0
	}
	if total <= 0 || supEquiv <= 0 {
		return 0
	}
	cells := barW * supEquiv / total
	if cells < 1 {
		cells = 1 // a nonzero supervisor share always shows
	}
	if cells > barW {
		cells = barW
	}
	return cells
}

func kv(w int, k, v string) string {
	gap := w - 2 - lipgloss.Width(k) - lipgloss.Width(v) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + sDim.Render(k) + strings.Repeat(" ", gap) + v + " "
}

func (m Model) viewTokensPanel(w int) string {
	wrkFresh, wrkCache := 0, 0
	for _, wk := range m.workers {
		wrkFresh += wk.In + wk.Out
		wrkCache += wk.CacheRd
	}

	// Headline numbers are FRESH tokens on both sides; prefix re-reads
	// get their own dim lines. The split bar is COST-WEIGHTED: a cached
	// input token is ~10% of a fresh one on API pricing, the best proxy
	// for what the plan actually meters — zero-weighting cache made the
	// leverage look ~4× better than it is. Supervisor totals include the
	// RUNNING turn's live estimate.
	supIn, supCacheRead, supCacheWrite, supOut := m.supTokens()
	freshSup := supIn + supCacheWrite + supOut
	supEquiv := freshSup + supCacheRead/10
	lines := []string{
		kv(w, "supervisor", sText.Render(formatTokens(freshSup)+" · ")+sAmberB.Render("plan")),
	}
	if supCacheRead > 0 {
		lines = append(lines, kv(w, "  cache reads", sFaint.Render(formatTokens(supCacheRead)+" · ~10% rate")))
	}
	// Context footprint: what EVERY future call re-reads — the number
	// that makes a bloated resumed session visible (and worth /new).
	if m.supCtx > 0 {
		v := sText.Render(formatTokens(m.supCtx))
		if m.ctxBloated() {
			v = sErr.Render(formatTokens(m.supCtx) + " · /new?")
		}
		lines = append(lines, kv(w, "  context", v))
	}
	lines = append(lines,
		kv(w, "workers", sText.Render(formatTokens(wrkFresh)+" · ")+sTealB.Render("$0.00")),
	)
	if wrkCache > 0 {
		lines = append(lines, kv(w, "  cache reads", sFaint.Render(formatTokens(wrkCache)+" · free")))
	}
	barW := w - 4
	if barW > 4 && supEquiv+wrkFresh > 0 {
		supCells := barCells(barW, supEquiv, wrkFresh)
		bar := sAmber.Render(strings.Repeat("▰", supCells)) + sTeal.Render(strings.Repeat("▰", barW-supCells))
		lines = append(lines, " "+bar+" ")
		supPct := 100 * supEquiv / (supEquiv + wrkFresh)
		legend := sDim.Render("≈plan-equiv ") + sAmberB.Render(fmt.Sprintf("%d%%", supPct))
		right := sTealB.Render(fmt.Sprintf("%d%%", 100-supPct)) + sDim.Render(" local")
		gap := w - 2 - lipgloss.Width(legend) - lipgloss.Width(right) - 2
		if gap < 1 {
			gap = 1
		}
		lines = append(lines, " "+legend+strings.Repeat(" ", gap)+right+" ")
		if supEquiv > 0 && wrkFresh > 0 {
			lines = append(lines, kv(w, "leverage",
				sTealB.Render(fmt.Sprintf("≈%.1fx", float64(wrkFresh)/float64(supEquiv)))+
					sFaint.Render(" · 1:1 assumption")))
		}
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
	// unreachable, or configured-but-not-served. Healthy idle models
	// collapse to one line naming them (a bare count reads as a riddle).
	var lines []string
	var idleNames []string
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
			idleNames = append(idleNames, ms.Name)
			continue
		}
		lines = append(lines, kv(w, truncPlain(ms.Name, w-16), val))
	}
	if len(idleNames) > 0 {
		label := strings.Join(idleNames, ", ")
		if len(label) > w-12 {
			label = fmt.Sprintf("%d models", len(idleNames))
		}
		lines = append(lines, kv(w, "idle", sFaint.Render(label)))
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
