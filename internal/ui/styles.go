// Package ui is the Bubble Tea TUI: tabs (chat, dashboard, logs), the
// worker table, and the token economy — layout, colors, and glyphs per
// docs/MOCKUP.html. External feeds are goroutines emitting typed tea.Msgs;
// the UI never talks to claude or opencode itself.
package ui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Palette from MOCKUP.html's CSS variables.
var (
	cTerm  = lipgloss.Color("#0F141B")
	cBord  = lipgloss.Color("#2A3441")
	cText  = lipgloss.Color("#C9D1D9")
	cDim   = lipgloss.Color("#7D8590")
	cFaint = lipgloss.Color("#4B5563")
	cAmber = lipgloss.Color("#D29922") // warnings / queued
	cTeal  = lipgloss.Color("#3FB950") // workers / local / unmetered
	cRun   = lipgloss.Color("#58A6FF") // status: running; accent
	cOK    = lipgloss.Color("#3FB950") // status: done
	cErr   = lipgloss.Color("#F85149") // status: failed
	cSel   = lipgloss.Color("#182231")
	cBrite = lipgloss.Color("#E6EDF3")
	cSup   = lipgloss.Color("#8B7CF6") // supervisor / plan-metered
	cFocus = lipgloss.Color("#4F7CFF") // focused pane border
	cKeyBg = lipgloss.Color("#1B2530")

	// panel border tints from the mock
	cSupBorder = lipgloss.Color("#3B3470")
	cWrkBorder = lipgloss.Color("#1F3A2A")
	cToastBrd  = lipgloss.Color("#5A2C28")
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
	sSup    = lipgloss.NewStyle().Foreground(cSup)
	sSupB   = lipgloss.NewStyle().Foreground(cSup).Bold(true)
	sRunB   = lipgloss.NewStyle().Foreground(cRun).Bold(true)

	sTabOn  = lipgloss.NewStyle().Foreground(cTerm).Background(cRun).Bold(true)
	sTabOff = lipgloss.NewStyle().Foreground(cDim)
	sKeycap = lipgloss.NewStyle().Foreground(cText).Background(cKeyBg).Bold(true)
	sSelRow = lipgloss.NewStyle().Background(cSel)
	sToast  = lipgloss.NewStyle().Foreground(cErr).Border(lipgloss.NormalBorder()).BorderForeground(cToastBrd).Padding(0, 1)
)

// Status glyphs from the mock. Box drawing and basic Unicode only — no
// emoji or icon-font glyphs, so every terminal font renders them.
const (
	glyphRun    = "●"
	glyphDone   = "✓"
	glyphFail   = "✗"
	glyphQueued = "◌"
	glyphStream = "●"
	glyphOut    = "→"
	glyphIn     = "←"
	glyphLogo   = "◉"
	glyphBell   = "▲"
)

// pill renders a title badge: text on a background colour with 1-cell
// padding, e.g. " ● claude-supervisor › ".
func pill(text string, fg, bg lipgloss.TerminalColor) string {
	return lipgloss.NewStyle().Foreground(fg).Background(bg).Bold(true).Render(" " + text + " ")
}

// pane draws a rounded box exactly w×h cells: a header row (head left,
// right right-aligned) then content lines, clipped/padded to the inner
// width. Extra lines past the box are dropped; missing ones are blank.
func pane(head, right string, lines []string, w, h int, border lipgloss.TerminalColor) string {
	if w < 8 {
		w = 8
	}
	if h < 3 {
		h = 3
	}
	inner := w - 2
	bs := lipgloss.NewStyle().Foreground(border)
	side := bs.Render("│")

	gap := inner - 2 - lipgloss.Width(head) - lipgloss.Width(right)
	header := " " + head
	if gap >= 1 {
		header += strings.Repeat(" ", gap) + right
	}
	rows := make([]string, 0, h)
	rows = append(rows, bs.Render("╭"+strings.Repeat("─", inner)+"╮"))
	rows = append(rows, side+padTo(header, inner)+side)
	for i := 0; i < h-3; i++ {
		ln := ""
		if i < len(lines) {
			ln = lines[i]
		}
		rows = append(rows, side+padTo(ln, inner)+side)
	}
	rows = append(rows, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(rows, "\n")
}

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

// ansiSeq matches CSI/OSC escape sequences in worker output.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)

// truncPlain shortens an unstyled string to n cells, stripping ANSI
// sequences and replacing control characters (worker output can carry
// escapes and carriage returns that would shred the layout) with spaces.
func truncPlain(s string, n int) string {
	if strings.ContainsRune(s, 0x1b) {
		s = ansiSeq.ReplaceAllString(s, "")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b.WriteRune(r)
	}
	s = b.String()
	// Measure in cells, not bytes: "⎇" is 3 bytes but 1 cell, and a byte
	// count cut labels that fit (and could split a rune).
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	b.Reset()
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > n-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
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
