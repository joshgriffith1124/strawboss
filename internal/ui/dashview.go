package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) viewDashboard(w, h int) string {
	metrics := m.viewMetrics(w)
	usedH := lipgloss.Height(metrics)

	tableRows := len(m.workers)
	if tableRows > 8 {
		tableRows = 8
	}
	table := m.viewWorkerTable(w, tableRows)
	usedH += lipgloss.Height(table)

	detailH := h - usedH - 2
	if detailH < 6 {
		detailH = 6
	}
	split := m.viewDetailSplit(w, detailH)

	return lipgloss.JoinVertical(lipgloss.Left, metrics, table, split)
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
		modelLines = append(modelLines,
			" "+sText.Render(ms.Name)+" "+sDim.Render(val)+" "+sFaint.Render(truncPlain(ms.Note, quarter-lipgloss.Width(ms.Name)-14)))
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

func (m Model) viewWorkerTable(w, maxRows int) string {
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
		if i == m.selected {
			cursor = sTealB.Render("▸")
		}
		row := fmt.Sprintf("%s%-4s %s %-8s %s %s %8s %8s ",
			cursor, wk.ID,
			statusGlyph(wk.Status, m.pulse), wk.Status,
			sTeal.Render(fmt.Sprintf("%-12s", truncPlain(wk.Model, 12))),
			taskStyle.Render(fmt.Sprintf("%-*s", w-54, truncPlain(task, w-54))),
			dur, toks)
		row = padTo(row, w-2)
		if i == m.selected {
			row = sSelRow.Render(row)
		}
		lines = append(lines, row)
	}
	return panel("Workers", lines, w, cBord, cDim)
}

func (m Model) viewDetailSplit(w, h int) string {
	leftW := w * 55 / 100
	rightW := w - leftW

	rows := m.visibleWorkers()
	var sel *workerRow
	if m.selected < len(rows) {
		sel = &rows[m.selected]
	}

	title := "Worker"
	var lines []string
	if sel != nil {
		title = fmt.Sprintf("Worker %s · %s · %s", sel.ID, sel.Model, sel.Status)

		// Throughput: live rate while output moves, lifetime average
		// otherwise (the live number naturally freezes between steps).
		tok := sText.Render(formatTokens(sel.In+sel.Out)) + sDim.Render(" tok")
		end := m.now
		if !sel.Ended.IsZero() {
			end = sel.Ended
		}
		elapsed := end.Sub(sel.Started)
		if r, ok := m.workerRates[sel.ID]; ok && sel.Status == "running" && r.rate > 0 && m.now.Sub(r.at) < 8*time.Second {
			tok += sDim.Render(" · ") + sTealB.Render(fmt.Sprintf("%.0f tok/s", r.rate)) + sDim.Render(" live")
		} else if sel.Out > 0 && elapsed > time.Second {
			tok += sDim.Render(fmt.Sprintf(" · avg %.0f tok/s", float64(sel.Out)/elapsed.Seconds()))
		}
		lines = append(lines, " "+tok)

		// Throughput history as a sparkline, once there's enough of it.
		if r, ok := m.workerRates[sel.ID]; ok && len(r.hist) >= 3 && leftW >= 24 {
			peak := 0.0
			for _, v := range r.hist {
				if v > peak {
					peak = v
				}
			}
			lines = append(lines, " "+sTeal.Render(sparkline(r.hist, leftW-16))+
				sDim.Render(fmt.Sprintf(" peak %.0f", peak)))
		}

		// Context footprint vs the model's window, when either is known.
		if sel.Ctx > 0 {
			ctx := sText.Render("ctx " + formatTokens(sel.Ctx))
			if win := m.contextWindowFor(sel.Model); win > 0 {
				pct := 100 * sel.Ctx / win
				style := sDim
				if pct >= 70 {
					style = sAmberB
				}
				ctx += sDim.Render(" / "+formatTokens(win)+" ") + style.Render(fmt.Sprintf("(%d%%)", pct))
			}
			lines = append(lines, " "+ctx)
		}

		meta := fmt.Sprintf("started %s · ran %s", sel.Started.Local().Format("15:04:05"), formatMinSec(elapsed))
		if sel.Steps > 0 {
			meta += fmt.Sprintf(" · step %d", sel.Steps)
		}
		if sel.Dir != "" {
			meta += " · " + sel.Dir
		}
		lines = append(lines, " "+sDim.Render(truncPlain(meta, leftW-4)))
		lines = append(lines, " "+sFaint.Render(truncPlain("log "+sel.LogPath, leftW-4)))
		// The full task lives nowhere else — every other surface truncates.
		if leftW >= 16 && strings.TrimSpace(sel.Task) != "" {
			wrapped := lipgloss.NewStyle().Width(leftW - 6).Render(sel.Task)
			for i, ln := range strings.Split(wrapped, "\n") {
				if i == 4 {
					lines = append(lines, " "+sFaint.Render("…"))
					break
				}
				lines = append(lines, " "+sFaint.Render(ln))
			}
		}
		evs := m.workerEvents[sel.ID]
		maxEv := h - 4 - len(lines) // room for the error + summary tail
		if maxEv < 1 {
			maxEv = 1
		}
		if len(evs) > maxEv {
			evs = evs[len(evs)-maxEv:]
		}
		for i, ev := range evs {
			branch := "├"
			if i == len(evs)-1 {
				branch = "└"
			}
			style := sDim
			switch ev.kind {
			case "tool":
				style = sText
			case "error":
				style = sErr
			case "reasoning":
				style = sFaint
			}
			lines = append(lines, " "+sFaint.Render(branch)+" "+style.Render(truncPlain(ev.text, leftW-8)))
		}
		if sel.Status == "failed" {
			// The last error the worker hit, straight from the transcript —
			// usually more specific than the summary.
			for i := len(m.workerEvents[sel.ID]) - 1; i >= 0; i-- {
				if ev := m.workerEvents[sel.ID][i]; ev.kind == "error" {
					lines = append(lines, " "+sErr.Render(truncPlain(glyphFail+" "+ev.text, leftW-4)))
					break
				}
			}
		}
		if sel.Status == "done" || sel.Status == "failed" {
			lines = append(lines, " "+sDim.Render(truncPlain("summary: "+firstLine(sel.Summary), leftW-4)))
		}
	} else {
		lines = []string{" " + sFaint.Render("select a worker (↑↓)")}
	}
	left := panel(title, padLines(lines, h-2), leftW, cWrkBorder, cTeal)

	dIn, dCacheR, dCacheW, dOut := m.supTokens()
	supIn := dIn + dCacheW // fresh input; cache reads separate
	cachePct := 0
	if total := supIn + dCacheR; total > 0 {
		cachePct = 100 * dCacheR / total
	}
	avgResult := 0
	for _, n := range m.delegationResultTokens {
		avgResult += n
	}
	if len(m.delegationResultTokens) > 0 {
		avgResult /= len(m.delegationResultTokens)
	}
	supLines := []string{
		" " + sDim.Render("auth ") + sAmber.Render(m.auth) + sDim.Render(" · marginal cost ") + sTeal.Render("$0.00"),
		" " + sDim.Render(fmt.Sprintf("fresh in %8s   cache-read %s (%d%%)", formatTokens(supIn), formatTokens(dCacheR), cachePct)),
		" " + sDim.Render(fmt.Sprintf("output   %8s", formatTokens(dOut))),
		" " + sDim.Render(fmt.Sprintf("notional API value $%.2f · avg %d tok/delegation result", m.supCost, avgResult)),
	}
	if m.supCtx > 0 {
		// MOCKUP.html shows "context    168k/200k" in this panel; the
		// denominator is the session model's own window, not a constant —
		// a 1M-context model read as 5x fuller than it was.
		denom := "?" // unknown until a result reports it; never a guess
		if w := m.ctxWindow(); w > 0 {
			denom = formatTokens(w)
		}
		ctx := " " + sDim.Render(fmt.Sprintf("context  %8s", formatTokens(m.supCtx)+"/"+denom))
		if m.ctxBloated() {
			ctx += " " + sErr.Render("— every call re-reads it all; /new starts fresh")
		}
		// Insert after the auth line so context sits with the identity of
		// the session it belongs to.
		supLines = append(supLines[:1], append([]string{ctx}, supLines[1:]...)...)
	}
	if m.fiveHour > 0 {
		supLines = append(supLines, " "+sDim.Render(fmt.Sprintf("plan window: 5h %.0f%% · 7d %.0f%%", m.fiveHour*100, m.sevenDay*100)))
	}
	if len(m.recentResults) > 0 {
		supLines = append(supLines, "", " "+sDim.Render("recent delegation results"))
		for _, r := range m.recentResults {
			style := sText
			if r.isError {
				style = sErr
			}
			supLines = append(supLines, "  "+style.Render(truncPlain(r.text, rightW-6)))
		}
	}
	right := panel("Supervisor detail", padLines(supLines, h-2), rightW, cSupBorder, cAmber)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
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
