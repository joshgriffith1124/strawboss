package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewModelsTab is the Models screen: a single pane filling w×h with the
// endpoint table — one row per models.toml entry, then any model seen
// only in the worker table — and, below it, a throughput sparkline for
// every model with a running worker.
func (m Model) viewModelsTab(w, h int) string {
	return pane(sDim.Render("MODELS"), "", m.modelsTabLines(w), w, h, cBord)
}

// modelCol is one fixed-width MODELS table column.
type modelCol struct {
	head  string
	width int
}

// modelCols are the table's columns in display order. At narrow widths
// the table keeps the longest leading prefix that still leaves room for
// the name, so SLOTS/CTX/RUN/… drop out before the name does.
var modelCols = []modelCol{
	{"STATUS", 6}, {"TOK/S", 5}, {"SLOTS", 12}, {"CTX", 6},
	{"RUN", 4}, {"DONE", 4}, {"FAIL", 4}, {"TOKENS", 6},
}

// modelsTabLines renders the MODELS pane content: a header row, one row
// per model, and the per-model sparklines. Every line is padded to the
// inner width; the pane clips the rest, so the box stays exactly w×h.
func (m Model) modelsTabLines(w int) []string {
	inner := w - 2
	if inner < 12 {
		inner = 12
	}

	// Row order: models.toml entries first (m.models order), then model
	// names seen only in the worker table, in worker order.
	names := make([]string, 0, len(m.models)+1)
	seen := make(map[string]bool, len(m.models))
	for _, ms := range m.models {
		names = append(names, ms.Name)
		seen[ms.Name] = true
	}
	for _, wk := range m.workers {
		if wk.Model != "" && !seen[wk.Model] {
			names = append(names, wk.Model)
			seen[wk.Model] = true
		}
	}

	if len(names) == 0 {
		return []string{" " + sFaint.Render("no model stats yet — models.toml entries appear here once polled")}
	}

	// Column set: the longest leading prefix that still leaves
	// minNameW cells for the name; the name itself then takes all the
	// room up to 20, and NOTE takes whatever the name leaves.
	const minNameW = 12
	cols := make([]modelCol, 0, len(modelCols))
	sum := 0 // Σ(separator + width) over the kept columns
	for _, c := range modelCols {
		if 1+minNameW+sum+1+c.width > inner {
			break
		}
		cols = append(cols, c)
		sum += 1 + c.width
	}
	nameW := inner - 1 - sum
	if nameW > 20 {
		nameW = 20
	}
	if nameW < 4 {
		nameW = 4
	}
	noteW := inner - 1 - sum - nameW
	hasNote := noteW >= 6

	head := " " + sDim.Render(padCell("NAME", nameW))
	for _, c := range cols {
		head += " " + sDim.Render(padCell(c.head, c.width))
	}
	if hasNote {
		head += " " + sFaint.Render("NOTE")
	}
	lines := []string{padTo(head, inner)}

	for _, name := range names {
		var stat modelStat
		inModels := false
		for _, ms := range m.models {
			if ms.Name == name {
				stat = ms
				inModels = true
				break
			}
		}
		run, done, fail, toks := 0, 0, 0, 0
		for _, wk := range m.workers {
			if wk.Model != name {
				continue
			}
			switch wk.Status {
			case "running":
				run++
			case "done":
				done++
			case "failed":
				fail++
			}
			toks += wk.In + wk.Out
		}

		// Endpoint state: a down note outweighs a busy endpoint; a
		// model seen only in the worker table has no stat yet.
		note := ""
		if inModels {
			note = stat.Note
		}
		low := strings.ToLower(note)
		status := sDim.Render("○ idle")
		switch {
		case strings.Contains(low, "not loaded") ||
			strings.Contains(low, "down") ||
			strings.Contains(low, "unreachable"):
			status = sErr.Render("✗ down")
		case inModels && stat.Active > 0:
			status = sOK.Render("● busy")
		}

		tokSec := "—"
		if inModels && stat.TokSec > 0 {
			tokSec = fmt.Sprintf("%.0f", stat.TokSec)
		}
		slots := "—"
		if inModels {
			slots = fmt.Sprintf("%d act · %d q", stat.Active, stat.Queue)
		}
		ctx := "—"
		if inModels && stat.ContextWindow > 0 {
			ctx = formatTokens(stat.ContextWindow)
		}
		tokCell := "—"
		if toks > 0 {
			tokCell = formatTokens(toks)
		}

		// vals[0] is the styled STATUS cell; the rest are plain text
		// padded to their column.
		vals := []string{
			status, tokSec, slots, ctx,
			fmt.Sprintf("%d", run), fmt.Sprintf("%d", done),
			fmt.Sprintf("%d", fail), tokCell,
		}
		line := " " + sText.Render(padCell(name, nameW))
		for i, c := range cols {
			cell := vals[i]
			if i > 0 {
				cell = padCell(vals[i], c.width)
			}
			line += " " + cell
		}
		if hasNote {
			line += " " + sFaint.Render(truncPlain(note, noteW))
		}
		lines = append(lines, padTo(line, inner))
	}

	// Throughput sparkline, one line per model with a running worker:
	// the longest rate history among that model's running workers.
	for _, name := range names {
		var hist []float64
		for _, wk := range m.workers {
			if wk.Model != name || wk.Status != "running" {
				continue
			}
			if r := m.workerRates[wk.ID]; len(r.hist) > len(hist) {
				hist = r.hist
			}
		}
		if len(hist) == 0 {
			continue
		}
		peak := 0.0
		for _, v := range hist {
			if v > peak {
				peak = v
			}
		}
		sparkW := 40
		// " peak NNN tok/s" at its widest is 16 cells.
		if max := inner - 1 - nameW - 2 - 16; max < sparkW {
			sparkW = max
		}
		if sparkW < 1 {
			sparkW = 1
		}
		line := " " + sText.Render(padCell(name, nameW)) + "  " +
			sTeal.Render(sparkline(hist, sparkW)) +
			sDim.Render(fmt.Sprintf(" peak %.0f tok/s", peak))
		lines = append(lines, padTo(line, inner))
	}

	return lines
}

// padCell pads a plain table cell to exactly w display cells, truncating
// with an ellipsis when it overflows.
func padCell(s string, w int) string {
	if w < 1 {
		return s
	}
	s = truncPlain(s, w)
	if wd := lipgloss.Width(s); wd < w {
		s += strings.Repeat(" ", w-wd)
	}
	return s
}
