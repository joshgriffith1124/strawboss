package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// viewDashboard is the Tasks screen: the worker table on top in a pane
// (about 55% of the height) with the task tally in its head, and below
// it the selected worker's live pane (left 60%) beside the DETAIL
// column holding that worker's facts.
func (m Model) viewDashboard(w, h int) string {
	topH := h * 55 / 100
	if topH < 3 {
		topH = 3
	}
	restH := h - topH
	if restH < 3 {
		restH = 3
	}

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
	// Worker rows that fit the table pane's content rows (topH-3),
	// leaving room for the header row and the filter line when a
	// filter is up.
	rows := topH - 4
	if m.filtering || m.filter != "" {
		rows--
	}
	if rows < 1 {
		rows = 1
	}
	top := pane(sDim.Render("TASKS"), taskTally(done, running, queued, failed),
		m.workerTableLines(w-2, rows), w, topH, cBord)

	leftW := w * 60 / 100
	rightW := w - leftW
	sel := m.selectedWorker()
	left := m.viewWorkerPane(sel, leftW, restH, true)
	detail := pane(sDim.Render("DETAIL"), "",
		padLines(m.workerDetailLines(sel, rightW-2), restH-3), rightW, restH, cWrkBorder)

	return lipgloss.JoinVertical(lipgloss.Left,
		top, lipgloss.JoinHorizontal(lipgloss.Top, left, detail))
}

func (m Model) viewMetrics(w int) string {
	quarter := w / 4
	// Fresh tokens headline; cache reads noted, never folded in. Totals
	// include the running turn's live estimate.
	supMIn, supMCacheR, supMCacheW, supMOut := m.supTokens()
	supFresh := supMIn + supMCacheW + supMOut
	wrkIn, wrkOut, active := 0, 0, 0
	for _, wk := range m.workers {
		wrkIn += wk.In
		wrkOut += wk.Out
		if wk.Status == "running" {
			active++
		}
	}

	sup := panel("● Supervisor · plan", []string{
		" " + sBoldT.Render(formatTokens(supFresh)+" fresh tok · ") + sAmberB.Render(fmt.Sprintf("$%.2f", 0.0)),
		" " + sDim.Render(fmt.Sprintf("%s auth · +%s cached · %d turns", m.auth, formatTokens(supMCacheR), m.supTurns)),
	}, quarter, cBord, cAmber)

	wrk := panel("● Workers · local", []string{
		" " + sBoldT.Render(formatTokens(wrkIn+wrkOut)+" tok · ") + sTealB.Render("$0.00"),
		" " + sDim.Render(fmt.Sprintf("%d active · %d sessions total", active, len(m.workers))),
	}, quarter, cBord, cTeal)

	var modelLines []string
	for _, ms := range m.models {
		val := "idle"
		if ms.TokSec > 0 {
			val = fmt.Sprintf("%.0f tok/s", ms.TokSec)
		}
		// Budget the note so the name and the stat (the point of the row)
		// keep their cells; at narrow widths the note drops out entirely
		// instead of shaving the stat.
		line := " " + sText.Render(ms.Name) + " " + sDim.Render(val)
		if noteW := quarter - lipgloss.Width(ms.Name) - lipgloss.Width(val) - 5; noteW > 0 {
			line += " " + sFaint.Render(truncPlain(ms.Note, noteW))
		}
		modelLines = append(modelLines, line)
	}
	if len(modelLines) == 0 {
		modelLines = []string{" " + sFaint.Render("no model stats")}
	}
	models := panel("Models", modelLines[:min(2, len(modelLines))], quarter, cBord, cDim)

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
	tasks := panel("Tasks", []string{
		" " + taskTally(done, running, queued, failed),
		" " + sDim.Render(fmt.Sprintf("queue depth %d", queued)),
	}, w-3*quarter, cBord, cDim)

	return lipgloss.JoinHorizontal(lipgloss.Top, sup, wrk, models, tasks)
}

// workerTableLines renders the worker table content: header, the filter
// line when one is active, and the rows — exactly what goes inside the
// panel today. The Agents screen's bottom Tasks panel reuses these lines.
func (m Model) workerTableLines(w, maxRows int) []string {
	head := fmt.Sprintf(" %-4s %-10s %-12s %-*s %8s %8s ",
		"ID", "STATUS", "MODEL", w-52, "TASK", "TIME", "TOKENS")
	lines := []string{sDim.Render(truncPlain(head, w-2))}
	if m.filtering {
		lines = append(lines, " "+m.filterInput.View())
	} else if m.filter != "" {
		lines = append(lines, " "+sTeal.Render("/ "+m.filter)+sFaint.Render("  (esc clears)"))
	}

	rows := m.visibleWorkers()
	if len(rows) == 0 {
		empty := "no workers yet — delegations appear here live"
		if m.filter != "" {
			empty = "no workers match / " + m.filter
		}
		lines = append(lines, " "+sFaint.Render(empty))
	}
	selIdx := m.selIndex(rows)
	for i, wk := range rows {
		if maxRows > 0 && i >= maxRows {
			lines = append(lines, " "+sFaint.Render(fmt.Sprintf("… %d more", len(rows)-maxRows)))
			break
		}
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
		task := wk.Task
		taskStyle := sText
		if wk.Status == "failed" && wk.Summary != "" {
			task = wk.Task + " — " + firstLine(wk.Summary)
			taskStyle = sErr
		}
		if wk.Status == "done" {
			taskStyle = sDim
		}
		cursor := " "
		if i == selIdx {
			cursor = sTealB.Render("▸")
		}
		row := fmt.Sprintf("%s%-4s %s %-8s %s %s %8s %8s ",
			cursor, wk.ID,
			statusGlyph(wk.Status, m.pulse), wk.Status,
			sTeal.Render(fmt.Sprintf("%-12s", truncPlain(wk.Model, 12))),
			taskStyle.Render(fmt.Sprintf("%-*s", w-54, truncPlain(task, w-54))),
			dur, toks)
		row = padTo(row, w-2)
		if i == selIdx {
			row = sSelRow.Render(row)
		}
		lines = append(lines, row)
	}
	return lines
}

// viewWorkerTable wraps workerTableLines in the Workers panel; output
// stays byte-identical to the pre-split rendering.
func (m Model) viewWorkerTable(w, maxRows int) string {
	return panel("Workers", m.workerTableLines(w, maxRows), w, cBord, cDim)
}

// workerDetailLines builds the Tasks screen's DETAIL column for one
// worker: token total with live/average rate, the throughput sparkline,
// context against the model window, run metadata, the log path, and the
// full task wrapped (max 6 lines). The transcript tail and the finished
// summary live in the worker's live pane instead.
func (m Model) workerDetailLines(wk *workerRow, w int) []string {
	if wk == nil {
		// Same one-cell left margin as the lines below.
		return []string{" " + sFaint.Render("select a worker (↑↓)")}
	}

	// Throughput: live rate while output moves, lifetime average
	// otherwise (the live number naturally freezes between steps).
	tok := sText.Render(formatTokens(wk.In+wk.Out)) + sDim.Render(" tok")
	end := m.now
	if !wk.Ended.IsZero() {
		end = wk.Ended
	}
	elapsed := end.Sub(wk.Started)
	if r, ok := m.workerRates[wk.ID]; ok && wk.Status == "running" && r.rate > 0 && m.now.Sub(r.at) < 8*time.Second {
		tok += sDim.Render(" · ") + sTealB.Render(fmt.Sprintf("%.0f tok/s", r.rate)) + sDim.Render(" live")
	} else if wk.Out > 0 && elapsed > time.Second {
		tok += sDim.Render(fmt.Sprintf(" · avg %.0f tok/s", float64(wk.Out)/elapsed.Seconds()))
	}
	lines := []string{" " + tok}

	// Throughput history as a sparkline, once there's enough of it.
	if r, ok := m.workerRates[wk.ID]; ok && len(r.hist) >= 3 && w >= 24 {
		peak := 0.0
		for _, v := range r.hist {
			if v > peak {
				peak = v
			}
		}
		lines = append(lines, " "+sTeal.Render(sparkline(r.hist, w-16))+
			sDim.Render(fmt.Sprintf(" peak %.0f", peak)))
	}

	// Context footprint vs the model's window, when either is known.
	if wk.Ctx > 0 {
		ctx := sText.Render("ctx " + formatTokens(wk.Ctx))
		if win := m.contextWindowFor(wk.Model); win > 0 {
			pct := 100 * wk.Ctx / win
			style := sDim
			if pct >= 70 {
				style = sAmberB
			}
			ctx += sDim.Render(" / "+formatTokens(win)+" ") + style.Render(fmt.Sprintf("(%d%%)", pct))
		}
		lines = append(lines, " "+ctx)
	}

	meta := fmt.Sprintf("started %s · ran %s", wk.Started.Local().Format("15:04:05"), formatMinSec(elapsed))
	if wk.Steps > 0 {
		meta += fmt.Sprintf(" · step %d", wk.Steps)
	}
	if wk.Dir != "" {
		meta += " · " + wk.Dir
	}
	lines = append(lines, " "+sDim.Render(truncPlain(meta, w-4)))
	lines = append(lines, " "+sFaint.Render(truncPlain("log "+wk.LogPath, w-4)))
	// The full task lives nowhere else — every other surface truncates.
	if w >= 16 && strings.TrimSpace(wk.Task) != "" {
		wrapped := lipgloss.NewStyle().Width(w - 6).Render(wk.Task)
		for i, ln := range strings.Split(wrapped, "\n") {
			if i == 5 {
				lines = append(lines, " "+sFaint.Render("…"))
				break
			}
			lines = append(lines, " "+sFaint.Render(ln))
		}
	}
	return lines
}

// sparkline renders rate samples as block glyphs, newest at the right.
func sparkline(vals []float64, width int) string {
	if width < 1 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	max := 0.0
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return ""
	}
	glyphs := []rune("▁▂▃▄▅▆▇█")
	out := make([]rune, len(vals))
	for i, v := range vals {
		idx := int(v / max * float64(len(glyphs)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(glyphs) {
			idx = len(glyphs) - 1
		}
		out[i] = glyphs[idx]
	}
	return string(out)
}

func padLines(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}
