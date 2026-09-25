package ui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// maxSideTiles is how many active workers stack in the column beside the
// supervisor pane; the next ones spill into a band below it.
const maxSideTiles = 3

// viewAgents renders the Agents screen: a 30-column agent sidebar on the
// right when the window is wide enough (or a one-line agent strip
// otherwise); the main area holds the supervisor pane with the first three
// active workers stacked beside it, a band of up to three more below, and
// the bottom Tasks/Logs/Metrics panel — or just the focused pane when
// zoomed. Finished workers leave the panes (the bottom Tasks table keeps
// them). It returns exactly h lines, each exactly w cells wide.
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
		switch m.focus {
		case focusSup:
			body = m.viewSupPane(mainW, mainH, true)
		case focusBottom:
			body = m.viewBottomPanel(mainW, mainH)
		default:
			body = m.viewWorkerPane(sel, mainW, mainH, true)
		}
	} else {
		active := m.activeWorkers()
		if m.hideTiles {
			active = nil // ctrl+t: the supervisor pane takes the full width
		}
		side, extra := active, []workerRow(nil)
		if len(side) > maxSideTiles {
			side, extra = active[:maxSideTiles], active[maxSideTiles:]
			if len(extra) > 3 {
				extra = extra[:3]
			}
		}

		// Band heights: the bottom panel owns 14 lines on tall screens
		// (8..h/3 on short ones), the overflow band gets 12 only when the
		// screen is tall and more than three workers are active, and the
		// top band takes the rest.
		bandBH := 0
		if len(extra) > 0 && h >= 40 {
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

		focused := func(wk *workerRow) bool {
			return m.focus == focusWork && sel != nil && wk.ID == sel.ID
		}

		// Top band: the supervisor pane, with the side tiles stacked in a
		// column to its right; alone at full width when nothing is active.
		// Too narrow for both: just the focused pane.
		var top string
		switch {
		case len(side) == 0:
			top = m.viewSupPane(mainW, bandAH, m.focus == focusSup)
		case mainW < 80:
			if m.focus == focusSup {
				top = m.viewSupPane(mainW, bandAH, true)
			} else {
				top = m.viewWorkerPane(sel, mainW, bandAH, true)
			}
		default:
			supW := mainW * 55 / 100
			colW := mainW - supW
			tileH := bandAH / len(side)
			tiles := make([]string, 0, len(side))
			for i := range side {
				th := tileH
				if i == len(side)-1 {
					th = bandAH - tileH*(len(side)-1)
				}
				tiles = append(tiles, m.viewWorkerPane(&side[i], colW, th, focused(&side[i])))
			}
			top = lipgloss.JoinHorizontal(lipgloss.Top,
				m.viewSupPane(supW, bandAH, m.focus == focusSup),
				strings.Join(tiles, "\n"))
		}

		bands := []string{top}
		if bandBH > 0 {
			// Overflow band: up to three more workers side by side in
			// equal widths, the last pane absorbing the remainder.
			paneW := mainW / len(extra)
			panes := make([]string, 0, len(extra))
			for i := range extra {
				ww := paneW
				if i == len(extra)-1 {
					ww = mainW - paneW*(len(extra)-1)
				}
				panes = append(panes, m.viewWorkerPane(&extra[i], ww, bandBH, focused(&extra[i])))
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
			if wk.Status == "done" {
				continue
			}
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

// activeWorkers are the workers that get a pane: running or queued, in
// spawn order (w1 before w2), so a tile keeps its place while its worker
// runs and the first three spawned fill the column beside the supervisor.
func (m Model) activeWorkers() []workerRow {
	var out []workerRow
	for _, wk := range m.workers {
		if wk.Status == "running" || wk.Status == "queued" {
			out = append(out, wk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return workerNum(out[i].ID) < workerNum(out[j].ID)
	})
	return out
}
