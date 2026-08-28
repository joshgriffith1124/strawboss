package ui

func (m Model) viewLogs(w, h int) string {
	inner := h - 2
	if inner < 1 {
		inner = 1
	}
	start := 0
	if len(m.logs) > inner {
		start = len(m.logs) - inner
	}
	var lines []string
	for _, ln := range m.logs[start:] {
		lines = append(lines, " "+sDim.Render(truncPlain(ln, w-4)))
	}
	if len(lines) == 0 {
		lines = []string{" " + sFaint.Render("raw feed lines appear here")}
	}
	return panel("Logs", padLines(lines, inner), w, cBord, cDim)
}
