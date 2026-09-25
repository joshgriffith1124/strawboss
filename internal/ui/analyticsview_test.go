package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// analyticsFixture builds a small Analytics state: an identified
// supervisor with committed usage, a live context footprint, plan-window
// utilisation, and one recent delegation result. When withScores, a
// three-model scorecard is attached and OnScorecard is wired.
func analyticsFixture(t *testing.T, w, h int, withScores bool) Model {
	t.Helper()
	base := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	m := New(nil)
	m = apply(t, m,
		tea.WindowSizeMsg{Width: w, Height: h},
		SupInitMsg{SessionID: "0123456789abcdef", Model: "claude-fable-5", Auth: "subscription", PID: 41772},
		SupRateLimitMsg{FiveHour: 0.04, SevenDay: 0.11},
		SupUsageMsg{Input: 1200, Output: 400, CacheRead: 14100, CacheWrite: 7200, CostUSD: 0.17, Turns: 1, CtxWindow: 200_000},
		SupTurnUsageMsg{Input: 400, CacheRead: 30_000, CacheWrite: 2_000},
		SupToolResultMsg{ToolID: "t1", Content: "w1 done 3m12s · log ~/.strawboss/logs/w1.log\nscaffold complete: 4 files"},
		WorkerUpsertMsg{ID: "w1", Model: "qwen-dsh", Task: "scaffold the app", Status: "done",
			Started: base, Ended: base.Add(3*time.Minute + 12*time.Second), Summary: "scaffold complete"},
	)
	if withScores {
		rows := []ScoreRow{
			{Model: "qwen-dsh", Attempts: 12, Done: 10, Failed: 2, Rescued: 1, Median: 3*time.Minute + 12*time.Second},
			{Model: "fast-local", Attempts: 8, Done: 4, Failed: 4, Rescued: 2, Median: 95 * time.Second},
			{Model: "glm-dsh", Attempts: 3, Done: 0, Failed: 3},
		}
		m.scores = rows
		m.OnScorecard = func() []ScoreRow { return rows }
	}
	return m
}

// TestViewAnalyticsTabShape: both panes must fill exactly w×h — h lines,
// every line exactly w cells wide — with and without a scorecard.
func TestViewAnalyticsTabShape(t *testing.T) {
	for _, withScores := range []bool{false, true} {
		for _, size := range [][2]int{{128, 45}, {60, 10}} {
			name := fmt.Sprintf("scores=%v/%dx%d", withScores, size[0], size[1])
			t.Run(name, func(t *testing.T) {
				m := analyticsFixture(t, size[0], size[1], withScores)
				out := m.viewAnalyticsTab(size[0], size[1])
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

// TestViewAnalyticsTabContent: the left pane carries the scorecard table
// (header, model rows, median, coloured OK%) and the right pane the
// supervisor's session numbers; the empty state depends on whether
// OnScorecard is wired.
func TestViewAnalyticsTabContent(t *testing.T) {
	m := analyticsFixture(t, 128, 45, true)
	plain := ansiSeq.ReplaceAllString(m.viewAnalyticsTab(128, 45), "")
	for _, want := range []string{
		"SCORECARD · all runs", "SESSION · token economy",
		"MODEL", "TASKS", "DONE", "FAIL", "OK%", "RESCUED", "MEDIAN",
		"qwen-dsh", "fast-local", "glm-dsh",
		"3m12s", "1m35s", "—",
		"subscription", "marginal cost", "$0.00",
		"fresh in", "cache-read", "notional API value",
		"context", "plan window: 5h 4% · 7d 11%",
		"recent delegation results", "w1 done 3m12s",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("analytics view missing %q", want)
		}
	}

	// No scorecard, handler wired: the plain empty state.
	m2 := analyticsFixture(t, 128, 45, false)
	m2.OnScorecard = func() []ScoreRow { return nil }
	out2 := ansiSeq.ReplaceAllString(m2.viewAnalyticsTab(128, 45), "")
	if !strings.Contains(out2, "no finished workers yet") {
		t.Errorf("wired empty state missing:\n%s", out2)
	}
	if strings.Contains(out2, "qwen-dsh") {
		t.Error("model rows should not render without scores")
	}

	// No scorecard, no handler: the demo-replay state.
	m3 := analyticsFixture(t, 128, 45, false)
	out3 := ansiSeq.ReplaceAllString(m3.viewAnalyticsTab(128, 45), "")
	if !strings.Contains(out3, "demo replay — no history") {
		t.Errorf("demo empty state missing:\n%s", out3)
	}
}

// TestViewAnalyticsTabNeverPanics: degenerate windows feed tiny or
// negative widths into the table and pane arithmetic; the panes clamp
// and clip, and the view must never crash.
func TestViewAnalyticsTabNeverPanics(t *testing.T) {
	for _, withScores := range []bool{false, true} {
		for _, size := range [][2]int{{0, 0}, {0, 7}, {7, 0}, {1, 1}, {8, 3}, {10, 4}, {40, 6}} {
			name := fmt.Sprintf("scores=%v/%dx%d", withScores, size[0], size[1])
			t.Run(name, func(t *testing.T) {
				m := analyticsFixture(t, size[0], size[1], withScores)
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("viewAnalyticsTab(%d,%d) panicked: %v", size[0], size[1], r)
					}
				}()
				_ = m.viewAnalyticsTab(size[0], size[1])
			})
		}
	}
}
