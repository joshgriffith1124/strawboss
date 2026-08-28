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
			b.WriteString(sRun.Render("YOU · "+it.when.Format("15:04")) + "\n")
			b.WriteString(wrap.Render(sBrite.Render(it.text)) + "\n\n")
		case "sup":
			b.WriteString(sAmber.Render("SUPERVISOR · "+it.when.Format("15:04")) + "\n")
			b.WriteString(wrap.Render(sText.Render(it.text)) + "\n\n")
		case "tool-out":
			b.WriteString(toolBlock(sAmber.Render(glyphOut), it.text, 240, sText, w))
		case "tool-in":
			mark := sOK.Render(glyphDone)
			if it.isError {
				mark = sErr.Render(glyphFail)
			}
			// Results get more room: the terse-result contract caps them,
			// and their content (summaries, denial reasons) is the point.
			b.WriteString(toolBlock(sTeal.Render(glyphIn)+" "+mark, it.text, 500, sDim, w))
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
		b.WriteString(wrap.Render(sText.Render(m.streaming.String())) + "\n")
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
	for len(lines) < logH {
		lines = append(lines, "")
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
// bounds runaway content (giant task prompts) before wrapping.
func toolBlock(prefix, text string, maxLen int, style lipgloss.Style, w int) string {
	text = truncPlain(text, maxLen)
	wrapped := lipgloss.NewStyle().Width(w - 6).Render(style.Render(text))
	lines := strings.Split(wrapped, "\n")
	var b strings.Builder
	b.WriteString("  " + prefix + " " + lines[0] + "\n")
	for _, ln := range lines[1:] {
		b.WriteString("      " + ln + "\n")
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
	supTotal := m.supIn + m.supCacheRead + m.supCacheWrite + m.supOut
	wrkIn, wrkOut := 0, 0
	for _, wk := range m.workers {
		wrkIn += wk.In
		wrkOut += wk.Out
	}
	wrkTotal := wrkIn + wrkOut

	lines := []string{
		kv(w, "supervisor", sText.Render(formatTokens(supTotal)+" · ")+sAmberB.Render("plan")),
		kv(w, "workers", sText.Render(formatTokens(wrkTotal)+" · ")+sTealB.Render("$0.00")),
	}

	// flow bar: supervisor vs worker share of all tokens moved
	barW := w - 4
	if barW > 4 && supTotal+wrkTotal > 0 {
		supCells := barW * supTotal / (supTotal + wrkTotal)
		if supCells < 1 && supTotal > 0 {
			supCells = 1
		}
		bar := sAmber.Render(strings.Repeat("▰", supCells)) + sTeal.Render(strings.Repeat("▰", barW-supCells))
		lines = append(lines, " "+bar+" ")
		supPct := 100 * supTotal / (supTotal + wrkTotal)
		legend := sAmberB.Render(fmt.Sprintf("%d%%", supPct)) + sDim.Render(" plan")
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
	return panel("Tokens", lines, w, cSupBorder, cAmber)
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
		mdl := truncPlain(wk.Model, 10)
		labelW := w - 24 - len(mdl) // prefix + right columns + 1 gap
		label := truncPlain(wk.Task, labelW)
		if wk.Status == "failed" {
			label = truncPlain(firstLine(wk.Summary), labelW)
		}
		labelStyle := sText
		if wk.Status == "failed" {
			labelStyle = sErr
		}
		left := fmt.Sprintf(" %-3s %s %s %s", wk.ID, statusGlyph(wk.Status, m.pulse),
			sTeal.Render(mdl), labelStyle.Render(label))
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
	var lines []string
	for _, ms := range m.models {
		val := "idle"
		if ms.TokSec > 0 {
			val = fmt.Sprintf("%.0f tok/s", ms.TokSec)
		}
		lines = append(lines, kv(w, ms.Name, sText.Render(val)))
		sub := ms.Note
		if ms.Active > 0 || ms.Queue > 0 {
			sub = fmt.Sprintf("%s · %d active · q%d", ms.Note, ms.Active, ms.Queue)
		}
		if sub != "" {
			lines = append(lines, " "+sFaint.Render(truncPlain(sub, w-4)))
		}
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
	tally := sOK.Render(fmt.Sprintf("%d%s", done, glyphDone)) + " " +
		sRun.Render(fmt.Sprintf("%d%s", running, glyphRun)) + " " +
		sDim.Render(fmt.Sprintf("%d%s", queued, glyphQueued)) + " " +
		sErr.Render(fmt.Sprintf("%d%s", failed, glyphFail))
	lines = append(lines, kv(w, "tasks", tally))
	return panel("Models", lines, w, cBord, cDim)
}
