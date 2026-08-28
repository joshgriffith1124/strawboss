package ui

import (
	"fmt"
	"strings"

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
	supTotal := m.supIn + m.supCacheRead + m.supCacheWrite + m.supOut
	cachePct := 0
	if in := m.supIn + m.supCacheRead + m.supCacheWrite; in > 0 {
		cachePct = 100 * m.supCacheRead / in
	}
	wrkIn, wrkOut, active := 0, 0, 0
	for _, wk := range m.workers {
		wrkIn += wk.In
		wrkOut += wk.Out
		if wk.Status == "running" {
			active++
		}
	}

	sup := panel("● Supervisor · plan", []string{
		" " + sBoldT.Render(formatTokens(supTotal)+" tok · ") + sAmberB.Render(fmt.Sprintf("$%.2f", 0.0)),
		" " + sDim.Render(fmt.Sprintf("%s auth · cache %d%% · %d turns", m.auth, cachePct, m.supTurns)),
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
		" " + sOK.Render(fmt.Sprintf("%d%s", done, glyphDone)) + " " +
			sRun.Render(fmt.Sprintf("%d%s", running, glyphRun)) + " " +
			sDim.Render(fmt.Sprintf("%d%s", queued, glyphQueued)) + " " +
			sErr.Render(fmt.Sprintf("%d%s", failed, glyphFail)),
		" " + sDim.Render(fmt.Sprintf("queue depth %d", queued)),
	}, w-3*quarter, cBord, cDim)

	return lipgloss.JoinHorizontal(lipgloss.Top, sup, wrk, models, tasks)
}

func (m Model) viewWorkerTable(w, maxRows int) string {
	head := fmt.Sprintf(" %-4s %-10s %-12s %-*s %8s %8s ",
		"ID", "STATUS", "MODEL", w-52, "TASK", "TIME", "TOKENS")
	lines := []string{sDim.Render(truncPlain(head, w-2))}

	rows := m.sortedWorkers()
	if len(rows) == 0 {
		lines = append(lines, " "+sFaint.Render("no workers yet — delegations appear here live"))
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
		row := fmt.Sprintf(" %-4s %s %-8s %s %s %8s %8s ",
			wk.ID,
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

	rows := m.sortedWorkers()
	var sel *workerRow
	if m.selected < len(rows) {
		sel = &rows[m.selected]
	}

	title := "Worker"
	var lines []string
	if sel != nil {
		title = fmt.Sprintf("Worker %s · %s · %s", sel.ID, sel.Model, sel.Status)
		lines = append(lines, " "+sDim.Render(truncPlain(
			fmt.Sprintf("opencode · %s tok · %s", formatTokens(sel.In+sel.Out), sel.LogPath), leftW-4)))
		evs := m.workerEvents[sel.ID]
		maxEv := h - 3
		if len(evs) > maxEv {
			evs = evs[len(evs)-maxEv:]
		}
		for i, ev := range evs {
			kind, text, _ := strings.Cut(ev, "\x00")
			branch := "├"
			if i == len(evs)-1 {
				branch = "└"
			}
			style := sDim
			switch kind {
			case "tool":
				style = sText
			case "error":
				style = sErr
			case "reasoning":
				style = sFaint
			}
			lines = append(lines, " "+sFaint.Render(branch)+" "+style.Render(truncPlain(text, leftW-8)))
		}
		if sel.Status == "done" || sel.Status == "failed" {
			lines = append(lines, " "+sDim.Render(truncPlain("summary: "+firstLine(sel.Summary), leftW-4)))
		}
	} else {
		lines = []string{" " + sFaint.Render("select a worker (↑↓)")}
	}
	left := panel(title, padLines(lines, h-2), leftW, cWrkBorder, cTeal)

	supTotal := m.supIn + m.supCacheRead + m.supCacheWrite
	cachePct := 0
	if supTotal > 0 {
		cachePct = 100 * m.supCacheRead / supTotal
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
		" " + sDim.Render(fmt.Sprintf("input   %8s   cache-read %s (%d%%)", formatTokens(supTotal), formatTokens(m.supCacheRead), cachePct)),
		" " + sDim.Render(fmt.Sprintf("output  %8s", formatTokens(m.supOut))),
		" " + sDim.Render(fmt.Sprintf("notional API value $%.2f · avg %d tok/delegation result", m.supCost, avgResult)),
	}
	if m.fiveHour > 0 {
		supLines = append(supLines, " "+sDim.Render(fmt.Sprintf("plan window: 5h %.0f%% · 7d %.0f%%", m.fiveHour*100, m.sevenDay*100)))
	}
	right := panel("Supervisor detail", padLines(supLines, h-2), rightW, cSupBorder, cAmber)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
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
