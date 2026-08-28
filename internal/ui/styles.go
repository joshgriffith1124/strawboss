// Package ui is the Bubble Tea TUI: tabs (chat, dashboard, logs), the
// worker table, and the token economy — layout, colors, and glyphs per
// docs/MOCKUP.html. External feeds are goroutines emitting typed tea.Msgs;
// the UI never talks to claude or opencode itself.
package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Palette from MOCKUP.html's CSS variables.
var (
	cTerm  = lipgloss.Color("#12161C")
	cBord  = lipgloss.Color("#26303B")
	cText  = lipgloss.Color("#C7D0DA")
	cDim   = lipgloss.Color("#5C6875")
	cFaint = lipgloss.Color("#39434F")
	cAmber = lipgloss.Color("#E2A84B") // supervisor / plan-metered
	cTeal  = lipgloss.Color("#43C6A8") // workers / local / unmetered
	cRun   = lipgloss.Color("#59B7E8") // status: running
	cOK    = lipgloss.Color("#66BE7E") // status: done
	cErr   = lipgloss.Color("#E0655C") // status: failed
	cSel   = lipgloss.Color("#1C232C")
	cBrite = lipgloss.Color("#E8EEF4")

	// panel border tints from the mock
	cSupBorder = lipgloss.Color("#4a3c22")
	cWrkBorder = lipgloss.Color("#1f4a40")
	cToastBrd  = lipgloss.Color("#5a2c28")
)

var (
	sText   = lipgloss.NewStyle().Foreground(cText)
	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sFaint  = lipgloss.NewStyle().Foreground(cFaint)
	sAmber  = lipgloss.NewStyle().Foreground(cAmber)
	sTeal   = lipgloss.NewStyle().Foreground(cTeal)
	sRun    = lipgloss.NewStyle().Foreground(cRun)
	sOK     = lipgloss.NewStyle().Foreground(cOK)
	sErr    = lipgloss.NewStyle().Foreground(cErr)
	sBrite  = lipgloss.NewStyle().Foreground(cBrite)
	sBoldT  = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sTealB  = lipgloss.NewStyle().Foreground(cTeal).Bold(true)
	sAmberB = lipgloss.NewStyle().Foreground(cAmber).Bold(true)

	sTabOn  = lipgloss.NewStyle().Foreground(cText).Background(cSel).Bold(true)
	sTabOff = lipgloss.NewStyle().Foreground(cDim)
	sKeycap = lipgloss.NewStyle().Foreground(cText).Background(cSel).Bold(true)
	sSelRow = lipgloss.NewStyle().Background(cSel)
	sToast  = lipgloss.NewStyle().Foreground(cErr).Border(lipgloss.NormalBorder()).BorderForeground(cToastBrd).Padding(0, 1)
)

// Status glyphs from the mock.
const (
	glyphRun    = "●"
	glyphDone   = "✔"
	glyphFail   = "✖"
	glyphQueued = "◌"
	glyphStream = "✻"
	glyphOut    = "→"
	glyphIn     = "←"
	glyphLogo   = "▚"
	glyphBell   = "🔔"
)

func statusGlyph(status string, pulse bool) string {
	switch status {
	case "running":
		if pulse {
			return sRun.Render(glyphRun)
		}
		return sFaint.Render(glyphRun)
	case "done":
		return sOK.Render(glyphDone)
	case "failed":
		return sErr.Render(glyphFail)
	case "queued":
		return sDim.Render(glyphQueued)
	default:
		return sDim.Render("?")
	}
}

// panel draws a box with the title embedded in the top border, mock-style:
// ┌─ TITLE ────┐. Content lines are clipped/padded to the inner width.
func panel(title string, lines []string, width int, border lipgloss.TerminalColor, titleColor lipgloss.TerminalColor) string {
	if width < 8 {
		width = 8
	}
	inner := width - 2
	bs := lipgloss.NewStyle().Foreground(border)
	ts := lipgloss.NewStyle().Foreground(titleColor)

	var b strings.Builder
	head := ""
	if title != "" {
		head = " " + strings.ToUpper(title) + " "
	}
	fill := inner - 1 - lipgloss.Width(head)
	if fill < 0 {
		fill = 0
	}
	b.WriteString(bs.Render("┌─") + ts.Render(head) + bs.Render(strings.Repeat("─", fill)+"┐") + "\n")
	for _, ln := range lines {
		b.WriteString(bs.Render("│") + padTo(ln, inner) + bs.Render("│") + "\n")
	}
	b.WriteString(bs.Render("└" + strings.Repeat("─", inner) + "┘"))
	return b.String()
}

// padTo clips or pads a styled line to exactly w display cells.
func padTo(s string, w int) string {
	cur := lipgloss.Width(s)
	if cur > w {
		return clipTo(s, w)
	}
	return s + strings.Repeat(" ", w-cur)
}

// clipTo truncates a styled string to w cells, ANSI-safely, by rebuilding
// through lipgloss width checks (coarse but dependency-free).
func clipTo(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	// Trim runes off the end until it fits; keeps escape sequences intact
	// because trailing resets are re-appended by lipgloss styles upstream.
	rs := []rune(s)
	for len(rs) > 0 && lipgloss.Width(string(rs)) > w-1 {
		rs = rs[:len(rs)-1]
	}
	return string(rs) + "…"
}

// truncPlain shortens an unstyled string to n cells.
func truncPlain(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatClock(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatMinSec(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}
