package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// lr lays out one row: left flush-left, right flush-right, inside exactly
// w cells with at least one cell of space between them. An over-wide left
// is clipped so the right label always keeps its cells.
func lr(w int, left, right string) string {
	if w < 1 {
		return ""
	}
	rw := lipgloss.Width(right)
	if rw >= w {
		return clipTo(right, w)
	}
	room := w - rw - 1
	if lipgloss.Width(left) > room {
		left = clipTo(left, room)
	}
	gap := w - lipgloss.Width(left) - rw
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}

// viewAgentSidebar is the Agents screen's right rail, matching the mockup's
// two stacked rounded boxes: the live agent roster (supervisor, then workers
// grouped by model) in the AGENTS pane, the fixed-height TOKEN ECONOMY pane
// below it. Exactly h lines, each w cells wide.
func (m Model) viewAgentSidebar(w, h int) string {
	// Mockup: TOKEN ECONOMY owns a fixed 13 rows (rows 32-44 of 45);
	// the AGENTS pane takes whatever is left, dropping roster rows from
	// the bottom when the window can't hold them.
	tokenH := 13
	agentsH := h - tokenH
	if agentsH < 3 {
		agentsH, tokenH = 3, h-3
	}
	inner := w - 2
	if inner < 1 {
		inner = 1
	}
	roster := m.agentRosterLines(inner, agentsH-3)
	agents := pane(sDim.Render(fmt.Sprintf("AGENTS (%d)", 1+len(m.listedWorkers()))), "",
		padLines(roster, agentsH-3), w, agentsH, cBord)
	economy := pane(sDim.Render("TOKEN ECONOMY (session)"), "",
		m.viewTokenEconomy(inner), w, tokenH, cBord)
	return lipgloss.JoinVertical(lipgloss.Left, agents, economy)
}

// viewTokenEconomy builds the rows of the TOKEN ECONOMY box (mockup right
// rail, TOKEN ECONOMY (session)). Every row is exactly w cells wide — the
// box's inner width — and the rounded box itself is drawn by the caller.
func (m Model) viewTokenEconomy(w int) []string {
	supIn, supCacheRead, supCacheWrite, supOut := m.supTokens()
	supFresh := supIn + supCacheWrite + supOut

	local := 0
	for _, wk := range m.workers {
		local += wk.In + wk.Out
	}

	// Offload bar: share of fresh tokens the workers produced locally.
	share := 0.0
	if total := local + supFresh; total > 0 {
		share = float64(local) / float64(total)
	}
	cells := int(share*float64(w) + 0.5)
	if cells > w {
		cells = w
	}
	bar := sOK.Render(strings.Repeat("█", cells)) + sFaint.Render(strings.Repeat("░", w-cells))

	planWindow := "—"
	if m.fiveHour > 0 || m.sevenDay > 0 {
		planWindow = fmt.Sprintf("5h %.0f%% · 7d %.0f%%", m.fiveHour*100, m.sevenDay*100)
	}

	avgResult := "—"
	if n := len(m.delegationResultTokens); n > 0 {
		sum := 0
		for _, t := range m.delegationResultTokens {
			sum += t
		}
		avgResult = fmt.Sprintf("%d tok", sum/n)
	}

	var thr float64
	for _, wk := range m.workers {
		if wk.Status != "running" {
			continue
		}
		thr += m.workerRates[wk.ID].rate
	}

	return []string{
		lr(w, sDim.Render("Plan (sup)"), sText.Render(formatTokens(supFresh)+"  $0.00")),
		lr(w, sDim.Render("Local (free)"), sTeal.Render(formatTokens(local)+"  $0.00")),
		lr(w, sDim.Render("Cached reads"), sText.Render(formatTokens(supCacheRead))),
		bar,
		lr(w, sDim.Render("Offload ratio"), sText.Render(fmt.Sprintf("%.1f%%", share*100))),
		lr(w, sDim.Render("Notional API value"), sText.Render(fmt.Sprintf("$%.2f", m.supCost))),
		lr(w, sDim.Render("Plan window"), sText.Render(planWindow)),
		lr(w, sDim.Render("Avg result"), sText.Render(avgResult)),
		lr(w, sDim.Render("Avg throughput"), sText.Render(fmt.Sprintf("%.0f tok/s", thr))),
	}
}

// listedWorkers are the workers the Agents roster lists: running, queued
// and failed. Done workers leave the page — they need no live row, and the
// Tasks tab keeps them for retry and undo; the roster reports their count
// in the model group header instead.
func (m Model) listedWorkers() []workerRow {
	var out []workerRow
	for _, wk := range m.sortedWorkers() {
		switch wk.Status {
		case "running", "queued", "failed":
			out = append(out, wk)
		}
	}
	return out
}

// agentRosterLines builds the Agents box content: the supervisor group,
// then one group per model that still has a listed worker (running,
// queued or failed — done workers are omitted, their count kept in the
// header). When the budget can't hold everyone, running workers and the
// newest failed keep their rows and the rest collapse to a faint
// "… N more" marker.
func (m Model) agentRosterLines(inner, budget int) []string {
	lines := []string{
		sDim.Render("● Supervisor"),
	}
	state := sDim.Render("idle")
	if m.streaming != "" || m.supStatus != "" {
		state = sSup.Render("● working")
	}
	lines = append(lines, lr(inner, sText.Render("claude-supervisor"), state))
	supIn, _, supCacheW, supOut := m.supTokens()
	supModel := m.supModel
	if supModel == "" {
		supModel = "—"
	}
	lines = append(lines, lr(inner, sDim.Render(supModel), sDim.Render(formatTokens(supIn+supCacheW+supOut)+" tok")))

	listed := m.listedWorkers()

	// Model groups: configured models in their panel order, then any
	// model a listed worker reports that list hasn't seen yet.
	seen := make(map[string]bool, len(m.models)+1)
	names := make([]string, 0, len(m.models)+1)
	for _, ms := range m.models {
		if !seen[ms.Name] {
			seen[ms.Name] = true
			names = append(names, ms.Name)
		}
	}
	groupName := func(model string) string {
		if model == "" {
			return "(none)"
		}
		return model
	}
	for _, wk := range listed {
		if n := groupName(wk.Model); !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}

	byModel := make(map[string][]workerRow, len(names))
	for _, wk := range listed {
		byModel[groupName(wk.Model)] = append(byModel[groupName(wk.Model)], wk)
	}

	// Done counts still show in the header even though the rows are gone.
	doneByModel := make(map[string]int)
	for _, wk := range m.workers {
		if wk.Status == "done" {
			doneByModel[groupName(wk.Model)]++
		}
	}

	// A group is drawn only while it still has a listed worker (running,
	// queued or failed — running workers are always listed).
	active := make([]string, 0, len(names))
	for _, n := range names {
		if len(byModel[n]) > 0 {
			active = append(active, n)
		}
	}

	// Budget: the fixed lines are the supervisor section plus one group
	// header per model; every other row is a worker pair.
	fixed := 3 + len(active)
	room := budget - fixed
	if room < 0 {
		room = 0
	}
	keep := make(map[string]bool, len(listed))
	if len(listed)*2 > room {
		var live, fin []workerRow
		for _, wk := range listed {
			if wk.Status == "running" || wk.Status == "queued" {
				live = append(live, wk)
			} else {
				fin = append(fin, wk) // failed only; done is not listed
			}
		}
		sort.SliceStable(fin, func(i, j int) bool {
			return !fin[i].Ended.IsZero() && fin[i].Ended.After(fin[j].Ended)
		})
		slots := room - 1 // reserve the "… N more" marker
		if slots < 0 {
			slots = 0
		}
		for _, wk := range append(live, fin...) {
			if len(keep) >= slots/2 {
				break
			}
			keep[wk.ID] = true
		}
	} else {
		for _, wk := range listed {
			keep[wk.ID] = true
		}
	}

	sel := m.selectedWorker()
	dropped := 0
	for _, n := range active {
		wks := byModel[n]
		running := 0
		for _, wk := range wks {
			if wk.Status == "running" {
				running++
			}
		}
		// Compact counts so the header fits the 30-column sidebar:
		// "● qwen-dsh  2● 1✓".
		head := sDim.Render("● "+n+"  ") + sRun.Render(fmt.Sprintf("%d%s", running, glyphRun))
		if done := doneByModel[n]; done > 0 {
			head += " " + sOK.Render(fmt.Sprintf("%d%s", done, glyphDone))
		}
		lines = append(lines, head)
		for _, wk := range wks {
			if !keep[wk.ID] {
				dropped++
				continue
			}
			isSel := sel != nil && sel.ID == wk.ID
			mark := " "
			if isSel {
				mark = sRunB.Render("▸")
			}
			toks := "—"
			if wk.In+wk.Out > 0 {
				toks = formatTokens(wk.In + wk.Out)
			}
			l1 := lr(inner, mark+sText.Render(wk.ID), statusGlyph(wk.Status, m.pulse)+" "+sDim.Render(wk.Status))
			l2 := lr(inner, sDim.Render(truncPlain(wk.Task, inner-8)), sDim.Render(toks))
			if isSel {
				l1 = sSelRow.Render(l1)
				l2 = sSelRow.Render(l2)
			}
			lines = append(lines, l1, l2)
		}
	}
	if dropped > 0 {
		lines = append(lines, sFaint.Render(fmt.Sprintf("… %d more", dropped)))
	}
	return lines
}

// viewBottomPanel is the Agents screen's bottom strip: a Tasks/Logs/
// Metrics tab row over the matching content, in a pane exactly h lines
// tall.
func (m Model) viewBottomPanel(w, h int) string {
	if h < 3 {
		h = 3
	}
	var head string
	for i, name := range []string{"Tasks", "Logs", "Metrics"} {
		if i > 0 {
			head += "  "
		}
		if i == m.bottomTab {
			head += sRunB.Underline(true).Render(name)
		} else {
			head += sFaint.Render(name)
		}
	}
	var body []string
	switch m.bottomTab {
	case bottomTasks:
		body = m.workerTableLines(w-2, h-4)
	case bottomLogs:
		body = m.logLines(w-2, h-3)
	case bottomMetrics:
		body = strings.Split(m.viewMetrics(w-2), "\n")
	}
	border, right := cBord, sFaint.Render("⇧⇥ to focus")
	if m.focus == focusBottom {
		border, right = cFocus, sDim.Render("←→ switch")
	}
	return pane(head, right, padLines(body, h-3), w, h, border)
}
