package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestInputWraps: typing past the column soft-wraps the input onto more
// rows (up to maxInputRows) instead of scrolling it sideways, every row
// fits the column, and no word is lost off-screen.
func TestInputWraps(t *testing.T) {
	words := strings.Fields("the quick brown fox jumps over the lazy dog while the supervisor waits for its workers to finish")
	tests := []struct {
		name     string
		value    string
		wantRows int
	}{
		{"empty", "", 1},
		{"short", "hello", 1},
		{"two rows", strings.Join(words[:8], " "), 2},
		{"capped", strings.Repeat(strings.Join(words, " ")+" ", 4), maxInputRows},
	}
	const w = 40
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(make(chan tea.Msg))
			m.input.SetValue(tt.value)
			rows := m.inputLines(w)
			if len(rows) != tt.wantRows {
				t.Fatalf("rows = %d, want %d:\n%s", len(rows), tt.wantRows, strings.Join(rows, "\n"))
			}
			for i, r := range rows {
				if lipgloss.Width(r) > w {
					t.Errorf("row %d is %d wide, column is %d: %q", i, lipgloss.Width(r), w, r)
				}
			}
			if tt.wantRows < maxInputRows {
				out := m.viewChatColumn(w, 20)
				for _, word := range strings.Fields(tt.value) {
					if !strings.Contains(out, word) {
						t.Errorf("word %q missing from the rendered input", word)
					}
				}
			}
		})
	}
}
