package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestWrapEventLinesHang: continuation lines start in the same column as
// the first line's text — no stray extra space for prefix-less events
// (reasoning, text), and hanging under the glyph's text for tool events.
func TestWrapEventLinesHang(t *testing.T) {
	text := "alpha bravo charlie delta echo foxtrot golf hotel india juliet"
	tests := []struct {
		name    string
		prefix  string
		textCol int // cells before the text on every line
	}{
		{"reasoning, no prefix", "", 1},
		{"tool glyph prefix", "✓ ", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := wrapEventLines(tt.prefix, lipgloss.NewStyle(), text, 16)
			if len(lines) < 2 {
				t.Fatalf("text did not wrap: %q", lines)
			}
			blank := strings.Repeat(" ", lipgloss.Width(tt.prefix))
			for i, ln := range lines {
				if i == 0 {
					ln = strings.Replace(ln, tt.prefix, blank, 1)
				}
				col := lipgloss.Width(ln) - lipgloss.Width(strings.TrimLeft(ln, " "))
				if col != tt.textCol {
					t.Errorf("line %d %q: text starts at col %d, want %d", i, ln, col, tt.textCol)
				}
			}
		})
	}
}
