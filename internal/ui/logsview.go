package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// wrkPalette gives each worker id a stable color so its lines are
// scannable in the interleave.
var wrkPalette = []lipgloss.Style{sTeal, sAmber, sOK, sBrite}

func (m Model) viewLogs(w, h int) string {
	inner := h - 2
	if inner < 1 {
		inner = 1
	}
	var filtered []logLine
	for _, ln := range m.logs {
		if m.logSrc == "" || ln.src == m.logSrc {
			filtered = append(filtered, ln)
		}
	}
	start := 0
	if len(filtered) > inner {
		start = len(filtered) - inner
	}
	var lines []string
	for _, ln := range filtered[start:] {
		style := sDim
		if ln.src == "wrk" {
			style = wrkStyle(ln.text)
		}
		lines = append(lines, " "+style.Render(truncPlain(ln.text, w-4)))
	}
	if len(lines) == 0 {
		empty := "raw feed lines appear here"
		if m.logSrc != "" {
			empty = "no " + m.logSrc + " lines yet (f cycles the filter)"
		}
		lines = []string{" " + sFaint.Render(empty)}
	}
	title := "Logs"
	if m.logSrc != "" {
		title = "Logs · " + m.logSrc
	}
	return panel(title, padLines(lines, inner), w, cBord, cDim)
}

// wrkStyle picks the stable color for a worker log line by its wN token.
func wrkStyle(text string) lipgloss.Style {
	for _, tok := range strings.Fields(text) {
		if len(tok) >= 2 && tok[0] == 'w' {
			n := 0
			digits := false
			for _, r := range tok[1:] {
				if r < '0' || r > '9' {
					digits = false
					break
				}
				digits = true
				n = n*10 + int(r-'0')
			}
			if digits {
				return wrkPalette[n%len(wrkPalette)]
			}
		}
	}
	return sDim
}
