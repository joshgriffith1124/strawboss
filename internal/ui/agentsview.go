package ui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewAgents renders the Agents screen: a 30-column agent sidebar on the
// right when the window is wide enough (or a one-line agent strip
// otherwise); the main area holds the supervisor and selected-worker
// panes on top, a band of up to three other workers, and the bottom
// Tasks/Logs/Metrics panel — or just the focused pane when zoomed.
// It returns exactly h lines, each exactly w cells wide.
func (m Model) viewAgents(w, h int) string {
	sidebar := w >= 120 && h >= 36
	mainW, mainH := w, h
	if sidebar {
		mainW -= 30
	} else {
		mainH-- // one-line agent strip
	}
	if mainH < 3 {
		mainH = 3
	}

	sel := m.selectedWorker()
	var body string
	if m.zoomed {
		// Zoomed: the focused pane alone fills the whole main area.
		if m.focus == focusSup {
			body = m.viewSupPane(mainW, mainH, true)
		} else {
			body = m.viewWorkerPane(sel, mainW, mainH, true)
		}
	} else {
		// Band heights: the bottom panel owns 14 lines on tall screens
		// (8..h/3 on short ones), the worker band gets 12 only when the
		// screen is tall and it has a worker to show, and the top band
		// takes the rest.
		others := m.bandBWorkers()
		bandBH := 0
		if len(others) > 0 && h >= 40 {
			bandBH = 12
		}
		bandCH := 14
		if h < 40 {
			bandCH = max(8, h/3)
		}
		bandAH := mainH - bandBH - bandCH
		if bandAH < 3 {
			// Degenerate height: keep the top pane at its 3-line
			// minimum and shrink the bottom panel to fit.
			bandAH = 3
			bandBH = 0
			bandCH = max(3, mainH-bandAH)
		}

		// Top band: supervisor pane plus the selected worker's pane,
		// or just the focused pane when too narrow for both.
		var top string
		if mainW < 100 {
			if m.focus == focusSup {
				top = m.viewSupPane(mainW, bandAH, true)
			} else {
				top = m.viewWorkerPane(sel, mainW, bandAH, true)
			}
		} else {
			supW := mainW * 56 / 98
			top = lipgloss.JoinHorizontal(lipgloss.Top,
				m.viewSupPane(supW, bandAH, m.focus == focusSup),
				m.viewWorkerPane(sel, mainW-supW, bandAH, m.focus == focusWork))
		}

		bands := []string{top}
		if bandBH > 0 {
			// Middle band: up to three worker panes side by side in
			// equal widths, the last pane absorbing the remainder.
			paneW := mainW / len(others)
			panes := make([]string, 0, len(others))
			for i, wk := range others {
				ww := paneW
				if i == len(others)-1 {
					ww = mainW - paneW*(len(others)-1)
				}
				panes = append(panes, m.viewWorkerPane(&wk, ww, bandBH, false))
			}
			bands = append(bands, lipgloss.JoinHorizontal(lipgloss.Top, panes...))
		}
		bands = append(bands, m.viewBottomPanel(mainW, bandCH))
		body = strings.Join(bands, "\n")
	}

	// Normalize line widths so overdrawn helper lines can't break the exactly-w×h contract.
	bodyLines := strings.Split(body, "\n")
	for i, ln := range bodyLines {
		bodyLines[i] = padTo(ln, mainW)
	}
	body = strings.Join(bodyLines, "\n")

	if !sidebar {
		sup := sDim.Render("sup idle")
		if m.streaming != "" || m.supStatus != "" {
			sup = sSup.Render("sup " + glyphRun + " working")
		}
		parts := []string{sup}
		for _, wk := range m.sortedWorkers() {
			parts = append(parts, sDim.Render(wk.ID)+" "+statusGlyph(wk.Status, m.pulse))
		}
		return padTo(strings.Join(parts, "  "), w) + "\n" + body
	}
	sideLines := strings.Split(m.viewAgentSidebar(30, h), "\n")
	for i, ln := range sideLines {
		sideLines[i] = padTo(ln, 30)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, body, strings.Join(sideLines, "\n"))
}

// bandBWorkers picks the middle-band workers: running ones first (newest
// Started first), then finished ones by most recent Ended, excluding the
// selected worker; at most 3.
func (m Model) bandBWorkers() []workerRow {
	sel := m.selectedWorker()
	var running, fin []workerRow
	for _, wk := range m.workers {
		if sel != nil && wk.ID == sel.ID {
			continue
		}
		switch wk.Status {
		case "running":
			running = append(running, wk)
		case "done", "failed":
			fin = append(fin, wk)
		}
	}
	sort.SliceStable(running, func(i, j int) bool {
		return running[i].Started.After(running[j].Started)
	})
	sort.SliceStable(fin, func(i, j int) bool {
		return fin[i].Ended.After(fin[j].Ended)
	})
	out := append(running, fin...)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
