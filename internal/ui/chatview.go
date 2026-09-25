package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewChatColumn renders the supervisor chat column: the log lines
// bottom-anchored, then the input — a faint rule over the input line, two
// rows in all. It is rendered inside the Agents screen's supervisor pane,
// whose own border is the box.
func (m Model) viewChatColumn(w, h int) string {
	// Callers budget this column from the window size (the supervisor
	// pane subtracts its border and margin), so a tiny window can hand
	// us a negative width that would reach the strings.Repeat on the
	// input rule below and panic outside any recover. Clamp to a
	// degenerate-but-valid column: garbage at 4 cells wide beats a
	// crashed TUI.
	if w < 4 {
		w = 4
	}
	if h < 4 {
		h = 4
	}
	inputH := 2
	attach := ""
	if len(m.pending) > 0 {
		attach = m.attachmentsLine(w)
		inputH++
	}
	hint := m.slashHint(w)
	if hint != "" {
		inputH++
	}
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
			b.WriteString(sSup.Render("SUPERVISOR · "+it.when.Local().Format("15:04")) + "\n")
			b.WriteString(wrap.Render(mdInline(hardWrap(it.text, w-4))) + "\n\n")
		case "tool-out":
			b.WriteString(toolBlock(sSup.Render(glyphOut), it.text, 240, sText, w))
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
		b.WriteString(sSup.Render("SUPERVISOR") + "\n")
		b.WriteString(wrap.Render(mdInline(hardWrap(m.streaming, w-4))) + "\n")
	}
	if m.supStatus != "" {
		star := glyphStream
		if m.pulse {
			star = sSup.Render(glyphStream)
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

	// Two input rows: a faint rule, then the prompt line. The value fills
	// the column beside the 2-cell "› " prompt plus the cursor's cell,
	// so the row stays exactly w wide.
	m.input.Width = w - 3
	rule := sFaint.Render(strings.Repeat("─", w))
	inputLine := m.input.View()

	if hint != "" {
		if attach != "" {
			return lipgloss.JoinVertical(lipgloss.Left, log, hint, attach, rule, inputLine)
		}
		return lipgloss.JoinVertical(lipgloss.Left, log, hint, rule, inputLine)
	}
	if attach != "" {
		return lipgloss.JoinVertical(lipgloss.Left, log, attach, rule, inputLine)
	}
	return lipgloss.JoinVertical(lipgloss.Left, log, rule, inputLine)
}

// attachmentsLine is the dim strip above the input box listing the pending
// pasted images; backspace on an empty input line removes the last one.
func (m Model) attachmentsLine(w int) string {
	var parts []string
	for i, img := range m.pending {
		parts = append(parts, fmt.Sprintf("[image %d · %d×%d · %d KB]", i+1, img.Width, img.Height, len(img.Data)/1024))
	}
	line := sDim.Render("attached: "+strings.Join(parts, " ")) + sFaint.Render("  backspace removes")
	if lipgloss.Width(line) > w {
		line = clipTo(line, w)
	}
	return line
}

// slashHint is the one-line completion strip shown above the input while
// a bare "/word" is being typed: the matching commands, first one bold
// (what Tab will pick), truncated to the column width.
func (m Model) slashHint(w int) string {
	matches := m.slashMatches()
	if len(matches) == 0 {
		return ""
	}
	// Fit by dropping whole entries (never slice a styled string): the
	// budget is the column minus the leader and a trailing " …".
	budget := w - 6
	var parts []string
	used := 0
	for i, c := range matches {
		if used+len(c)+3 > budget {
			parts = append(parts, sFaint.Render("…"))
			break
		}
		used += len(c) + 3
		if i == 0 {
			parts = append(parts, sTealB.Render(c))
		} else {
			parts = append(parts, sTeal.Render(c))
		}
	}
	return " " + sFaint.Render("⇥ ") + strings.Join(parts, sFaint.Render(" · "))
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
	// panel() embeds " TITLE " in the top border; when the title is
	// wider than w-5 that border row overflows w and the whole box is
	// drawn over-wide. Truncate the plain title so the panel stays
	// exactly w cells wide (at the dashboard's 38-column panel it
	// fits whole and renders unchanged).
	title := "Tokens · plan vs free local"
	if lipgloss.Width(title) > w-5 {
		title = truncPlain(title, w-5)
	}
	return panel(title, lines, w, cSupBorder, cAmber)
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
