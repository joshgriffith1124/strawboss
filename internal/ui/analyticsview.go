package ui

import (
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// viewAnalyticsTab is the Analytics screen: the historical per-model
// scorecard over ALL runs (left, 55%) beside the supervisor session's
// token economy (right, the rest). Both panes fill the whole w×h.
func (m Model) viewAnalyticsTab(w, h int) string {
	leftW := w * 55 / 100
	rightW := w - leftW

	left := pane(sDim.Render("SCORECARD · all runs"), "", m.scorecardLines(leftW), leftW, h, cBord)
	right := pane(sDim.Render("SESSION · token economy"), "", m.supervisorDetailLines(rightW), rightW, h, cSupBorder)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// scorecardLines renders the per-model scorecard table (m.scores): a dim
// header plus one line per model, OK% coloured by outcome (green ≥80,
// amber ≥50, red below). The model name takes whatever width is left
// after the fixed numeric columns; the pane clips the overflow.
func (m Model) scorecardLines(w int) []string {
	const (
		numTasks   = 5
		numDone    = 5
		numFail    = 5
		numOK      = 4 // "100%"
		numRescued = 7 // "RESCUED"
		numMedian  = 9 // "1h02m03s"
	)
	inner := w - 2
	modelW := inner - (numTasks + numDone + numFail + numOK + numRescued + numMedian + 6)
	if modelW < 4 {
		modelW = 4
	}
	head := fmt.Sprintf("%-*s %5s %5s %5s %4s %7s %9s",
		modelW, "MODEL", "TASKS", "DONE", "FAIL", "OK%", "RESCUED", "MEDIAN")
	lines := []string{sDim.Render(truncPlain(head, inner))}
	if len(m.scores) == 0 {
		empty := "no finished workers yet"
		if m.OnScorecard == nil {
			empty = "demo replay — no history"
		}
		lines = append(lines, " "+sFaint.Render(empty))
		return lines
	}
	for _, r := range m.scores {
		pct := 0
		if r.Attempts > 0 {
			pct = 100 * r.Done / r.Attempts
		}
		okStyle := sErr
		switch {
		case pct >= 80:
			okStyle = sOK
		case pct >= 50:
			okStyle = sAmber
		}
		median := "—"
		if r.Median > 0 {
			median = r.Median.Round(time.Second).String()
		}
		line := fmt.Sprintf("%-*s %5d %4d %4d %s %5d %9s",
			modelW, truncPlain(r.Model, modelW), r.Attempts, r.Done, r.Failed,
			okStyle.Render(fmt.Sprintf("%4d%%", pct)), r.Rescued, median)
		lines = append(lines, padTo(line, inner))
	}
	return lines
}

// modelCostLines splits the notional API value by model, costliest first:
// the session model and the small helper model the CLI runs alongside it.
// Shown only once a result has reported a split.
func (m Model) modelCostLines(w int) []string {
	if len(m.supModelCost) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.supModelCost))
	total := 0.0
	for name, c := range m.supModelCost {
		names = append(names, name)
		total += c
	}
	sort.Slice(names, func(i, j int) bool {
		ci, cj := m.supModelCost[names[i]], m.supModelCost[names[j]]
		if ci != cj {
			return ci > cj
		}
		return names[i] < names[j]
	})
	lines := make([]string, 0, len(names))
	for _, name := range names {
		c := m.supModelCost[name]
		pct := 0
		if total > 0 {
			pct = int(100*c/total + 0.5)
		}
		suffix := fmt.Sprintf(" $%.2f (%d%%)", c, pct)
		lines = append(lines, "   "+sFaint.Render(truncPlain(name, w-6-len(suffix))+suffix))
	}
	return lines
}

// supervisorDetailLines builds the supervisor session's token-economy
// lines: auth + marginal cost, context footprint, fresh-in / cache-read
// split, output, notional API value and average result size, plan-window
// utilisation, and the recent delegation results. Shared by the
// dashboard's detail split and the Analytics tab's right pane.
func (m Model) supervisorDetailLines(w int) []string {
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
	lines := []string{
		" " + sDim.Render("auth ") + sAmber.Render(m.auth) + sDim.Render(" · marginal cost ") + sTeal.Render("$0.00"),
		" " + sDim.Render(fmt.Sprintf("fresh in %8s   cache-read %s (%d%%)", formatTokens(supIn), formatTokens(dCacheR), cachePct)),
		" " + sDim.Render(fmt.Sprintf("output   %8s", formatTokens(dOut))),
		" " + sDim.Render(fmt.Sprintf("notional API value $%.2f · avg %d tok/delegation result", m.supCost, avgResult)),
	}
	lines = append(lines, m.modelCostLines(w)...)
	if m.supCtx > 0 {
		// MOCKUP.html shows "context    168k/200k" in this panel; the
		// denominator is the session model's own window, not a constant —
		// a 1M-context model read as 5x fuller than it was.
		denom := "?" // unknown until a result reports it; never a guess
		if cw := m.ctxWindow(); cw > 0 {
			denom = formatTokens(cw)
		}
		ctx := " " + sDim.Render(fmt.Sprintf("context  %8s", formatTokens(m.supCtx)+"/"+denom))
		if m.ctxBloated() {
			ctx += " " + sErr.Render("— every call re-reads it all; /new starts fresh")
		}
		// Insert after the auth line so context sits with the identity of
		// the session it belongs to.
		lines = append(lines[:1], append([]string{ctx}, lines[1:]...)...)
	}
	if m.fiveHour > 0 {
		lines = append(lines, " "+sDim.Render(fmt.Sprintf("plan window: 5h %.0f%% · 7d %.0f%%", m.fiveHour*100, m.sevenDay*100)))
	}
	if len(m.recentResults) > 0 {
		lines = append(lines, "", " "+sDim.Render("recent delegation results"))
		for _, r := range m.recentResults {
			style := sText
			if r.isError {
				style = sErr
			}
			lines = append(lines, "  "+style.Render(truncPlain(r.text, w-6)))
		}
	}
	return lines
}
