package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// modelsFixture builds a Models tab state: three models.toml entries
// (a busy one, an idle one, a down one) plus four workers across them
// and one running worker on a model with no stat row at all — the state
// the Models screen must draw at any size without blowing its frame.
// The running workers get rate histories directly: usage deltas would
// divide token counts by nanoseconds in a test.
func modelsFixture(t *testing.T, w, h int) Model {
	t.Helper()
	base := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	m := apply(t, New(nil),
		tea.WindowSizeMsg{Width: w, Height: h},
		ModelStatMsg{Name: "qwen-dsh", TokSec: 74, Active: 2, Queue: 1, Note: "dsh · local", ContextWindow: 131_072},
		ModelStatMsg{Name: "fast-local", Note: "vllm · single"},
		ModelStatMsg{Name: "glm-dsh", Note: "endpoint unreachable"},
		WorkerUpsertMsg{ID: "w1", Model: "qwen-dsh", Task: "scaffold the app", Status: "running", Started: base},
		WorkerUpsertMsg{ID: "w2", Model: "fast-local", Task: "docstrings", Status: "done",
			Started: base, Ended: base.Add(3 * time.Minute), Summary: "docstrings added"},
		WorkerUpsertMsg{ID: "w3", Model: "qwen-dsh", Task: "write the tests", Status: "failed",
			Started: base, Ended: base.Add(time.Minute), Summary: "harness timeout"},
		WorkerUpsertMsg{ID: "w4", Model: "opus-edge", Task: "refactor the builder", Status: "running", Started: base.Add(time.Minute)},
		WorkerUsageMsg{ID: "w1", Input: 30000, Output: 8200},
		WorkerUsageMsg{ID: "w3", Input: 10000, Output: 4000},
	)
	m.workerRates["w1"] = workerRate{rate: 74, hist: []float64{12, 30, 66, 90, 74}}
	m.workerRates["w4"] = workerRate{rate: 18, hist: []float64{8, 18}}
	return m
}

// TestViewModelsTabShape: the pane must fill exactly w×h — h lines,
// every line exactly w cells wide — with and without data.
func TestViewModelsTabShape(t *testing.T) {
	for _, withData := range []bool{false, true} {
		for _, size := range [][2]int{{128, 45}, {60, 10}} {
			name := fmt.Sprintf("data=%v/%dx%d", withData, size[0], size[1])
			t.Run(name, func(t *testing.T) {
				var m Model
				if withData {
					m = modelsFixture(t, size[0], size[1])
				} else {
					m = apply(t, New(nil), tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				}
				out := m.viewModelsTab(size[0], size[1])
				lines := strings.Split(out, "\n")
				if len(lines) != size[1] {
					t.Fatalf("returned %d lines, want %d\nout:\n%s", len(lines), size[1], out)
				}
				for i, ln := range lines {
					if w := lipgloss.Width(ln); w != size[0] {
						t.Errorf("line %d width %d, want %d: %.80q", i, w, size[0], ln)
					}
				}
			})
		}
	}
}

// TestViewModelsTabContent: at full width the table carries every column
// (name, status, tok/s, slots, ctx, run/done/fail, tokens, note), the
// worker-only model gets its own row, and the running models get a
// sparkline with a peak below the table.
func TestViewModelsTabContent(t *testing.T) {
	m := modelsFixture(t, 128, 45)
	plain := ansiSeq.ReplaceAllString(m.viewModelsTab(128, 45), "")

	for _, want := range []string{
		"MODELS",
		// header
		"NAME", "STATUS", "TOK/S", "SLOTS", "CTX", "RUN", "DONE", "FAIL", "TOKENS", "NOTE",
		// busy endpoint: stat + worker aggregates + note
		"qwen-dsh", "● busy", "74", "2 act · 1 q", "131.1k", "dsh · local",
		// idle endpoint: no workers, no stat movement
		"fast-local", "○ idle", "vllm · single",
		// down endpoint: the note outweighs everything
		"glm-dsh", "✗ down",
		// a model seen only in the worker table
		"opus-edge",
		// sparklines: longest history per running model, with its peak
		// (hist [12 30 66 90 74] scaled against 90 → ▁▃▆█▆)
		"peak 90 tok/s", "peak 18 tok/s",
		"▁▃▆█▆",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("models view missing %q:\n%s", want, plain)
		}
	}

	// qwen-dsh's TOKENS column sums that model's workers' In+Out:
	// w1's 38.2k plus failed w3's 14k = 52.2k.
	if !strings.Contains(plain, "52.2k") {
		t.Errorf("models view missing worker token sum:\n%s", plain)
	}
}

// TestViewModelsTabNarrow: at 60 wide the fixed columns drop out right
// to left (TOKENS, then FAIL, …) while the name and status survive, and
// the empty state renders on its own.
func TestViewModelsTabNarrow(t *testing.T) {
	m := modelsFixture(t, 60, 10)
	plain := ansiSeq.ReplaceAllString(m.viewModelsTab(60, 10), "")
	for _, want := range []string{"qwen-dsh", "● busy", "○ idle", "✗ down"} {
		if !strings.Contains(plain, want) {
			t.Errorf("narrow models view missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "TOKENS") {
		t.Errorf("TOKENS column should drop out at 60 wide:\n%s", plain)
	}

	empty := apply(t, New(nil), tea.WindowSizeMsg{Width: 60, Height: 10})
	plain = ansiSeq.ReplaceAllString(empty.viewModelsTab(60, 10), "")
	// The full message clips at 60 wide; assert on the prefix.
	if !strings.Contains(plain, "no model stats yet — models.toml entries appear here") {
		t.Errorf("empty state missing:\n%s", plain)
	}
}

// TestViewModelsTabNeverPanics: degenerate windows (0×0 down to 1×1)
// feed tiny or negative widths into the table and pane arithmetic; the
// pane clamps and clips, and the view must never crash.
func TestViewModelsTabNeverPanics(t *testing.T) {
	for _, withData := range []bool{false, true} {
		for _, size := range [][2]int{{0, 0}, {1, 1}, {0, 7}, {7, 0}, {8, 3}, {10, 4}, {30, 6}} {
			name := fmt.Sprintf("data=%v/%dx%d", withData, size[0], size[1])
			t.Run(name, func(t *testing.T) {
				var m Model
				if withData {
					m = modelsFixture(t, size[0], size[1])
				} else {
					m = apply(t, New(nil), tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				}
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("viewModelsTab(%d,%d) panicked: %v", size[0], size[1], r)
					}
				}()
				_ = m.viewModelsTab(size[0], size[1])
			})
		}
	}
}
