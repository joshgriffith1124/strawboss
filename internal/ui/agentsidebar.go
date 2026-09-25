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

// viewAgentSidebar is the Agents screen's right rail: the live agent
// roster (supervisor, then workers grouped by model) above the existing
// token panel. Exactly h lines, each w cells wide.
func (m Model) viewAgentSidebar(w, h int) string {
	tokens := m.viewTokensPanel(w)
	agentsH := h - lipgloss.Height(tokens)
	if agentsH < 4 {
		agentsH = 4
	}
	inner := w - 2
	if inner < 1 {
		inner = 1
	}
	lines := m.agentRosterLines(inner, agentsH-2)
	box := panel(fmt.Sprintf("Agents (%d)", 1+len(m.workers)), padLines(lines, agentsH-2), w, cBord, cSup)
	return lipgloss.JoinVertical(lipgloss.Left, box, tokens)
}

// agentRosterLines builds the Agents box content: the supervisor group,
// then one group per model. When the budget can't hold everyone, running
// workers and the newest finished keep their rows and the rest collapse
// to a faint "… N more" marker.
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

	// Model groups: configured models in their panel order, then any
	// model a worker reports that list hasn't seen yet.
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
	for _, wk := range m.workers {
		if n := groupName(wk.Model); !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}

	byModel := make(map[string][]workerRow, len(names))
	for _, wk := range m.sortedWorkers() {
		byModel[groupName(wk.Model)] = append(byModel[groupName(wk.Model)], wk)
	}

	// Budget: the fixed lines are the supervisor section plus one group
	// header per model; every other row is a worker pair.
	fixed := 3 + len(names)
	room := budget - fixed
	if room < 0 {
		room = 0
	}
	keep := make(map[string]bool, len(m.workers))
	if len(m.workers)*2 > room {
		var live, fin []workerRow
		for _, wk := range m.workers {
			if wk.Status == "running" || wk.Status == "queued" {
				live = append(live, wk)
			} else {
				fin = append(fin, wk)
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
		for _, wk := range m.workers {
			keep[wk.ID] = true
		}
	}

	sel := m.selectedWorker()
	dropped := 0
	for _, n := range names {
		wks := byModel[n]
		running := 0
		for _, wk := range wks {
			if wk.Status == "running" {
				running++
			}
		}
		lines = append(lines, sDim.Render("● "+n+fmt.Sprintf(" · %d running", running)))
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
	return pane(head, "", padLines(body, h-3), w, h, cBord)
}
