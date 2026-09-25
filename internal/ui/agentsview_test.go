package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// agentsFeed pushes msgs through Update, the same way model_test.go's
// apply does (kept local so this file stays independent of that one).
func agentsFeed(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		var ok bool
		m, ok = next.(Model)
		if !ok {
			t.Fatalf("Update returned %T", next)
		}
	}
	return m
}

// agentsFixture builds a fresh model at w×h with the supervisor
// identified and, when nWorkers > 0, a running/queued/done/failed mix of
// five workers with usage and transcript events — the state the Agents
// screen must draw without blowing its frame.
func agentsFixture(t *testing.T, w, h, nWorkers int) Model {
	t.Helper()
	base := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	msgs := []tea.Msg{
		tea.WindowSizeMsg{Width: w, Height: h},
		SupInitMsg{SessionID: "0123456789abcdef", Model: "claude-fable-5", Auth: "subscription", PID: 41772},
		ModelStatMsg{Name: "qwen-coder", TokSec: 74, Active: 2},
		ModelStatMsg{Name: "qwen-small"},
		SupRateLimitMsg{FiveHour: 0.04, SevenDay: 0.11},
	}
	if nWorkers > 0 {
		msgs = append(msgs,
			SupUserMsg{Text: "Build the trade query builder module.", Time: base},
			SupTextDoneMsg{Text: "Splitting this into four tasks.", Time: base},
			SupUsageMsg{Input: 1200, Output: 400, CacheRead: 14100, CacheWrite: 7200, CostUSD: 0.17, Turns: 1},
			WorkerUpsertMsg{ID: "w1", Model: "qwen-coder", Task: "scaffold the app", Status: "running", Started: base},
			WorkerUpsertMsg{ID: "w2", Model: "qwen-small", Task: "docstrings", Status: "queued", Started: base},
			WorkerUpsertMsg{ID: "w3", Model: "qwen-coder", Task: "write the tests", Status: "done", Started: base.Add(time.Minute), Ended: base.Add(4 * time.Minute), Summary: "all tests pass"},
			WorkerUpsertMsg{ID: "w4", Model: "qwen-small", Task: "lint the package", Status: "failed", Started: base.Add(2 * time.Minute), Summary: "lint: 2 errors"},
			WorkerUpsertMsg{ID: "w5", Model: "qwen-coder", Task: "refactor the query builder", Status: "running", Started: base.Add(3 * time.Minute)},
			WorkerEventMsg{ID: "w1", Kind: "tool", Text: "bash go build ./..."},
			WorkerEventMsg{ID: "w1", Kind: "text", Text: "Scaffolding the app package."},
			WorkerEventMsg{ID: "w3", Kind: "tool", Text: "bash go test ./... [completed]"},
			WorkerEventMsg{ID: "w4", Kind: "error", Text: "lint: 2 errors"},
			WorkerUsageMsg{ID: "w1", Input: 30000, Output: 8200},
			WorkerUsageMsg{ID: "w3", Input: 10000, Output: 4000},
			WorkerUsageMsg{ID: "w4", Input: 5000, Output: 1200},
		)
	}
	return agentsFeed(t, New(nil), msgs...)
}

// TestAgentsViewNeverPanicsAtTinySizes: degenerate windows (0×0 down to
// 1×1) feed negative widths into the pane arithmetic — viewSupPane hands
// the chat column w-3, and at a 1-column window that reaches the input
// rule's strings.Repeat and panics outside any recover. Tiny screens may
// render garbage, but View must never crash.
func TestAgentsViewNeverPanicsAtTinySizes(t *testing.T) {
	sizes := [][2]int{{0, 0}, {1, 0}, {1, 1}, {5, 3}, {20, 5}, {40, 10}}
	for _, size := range sizes {
		for _, nWorkers := range []int{0, 5} {
			for _, focus := range []int{focusSup, focusWork, focusBottom} {
				for _, zoomed := range []bool{false, true} {
					name := fmt.Sprintf("%dx%d/%dworkers/focus=%s/zoomed=%v",
						size[0], size[1], nWorkers,
						map[int]string{focusSup: "sup", focusWork: "work", focusBottom: "bottom"}[focus],
						zoomed)
					t.Run(name, func(t *testing.T) {
						m := agentsFixture(t, size[0], size[1], nWorkers)
						m.tab = tabChat
						m.focus = focus
						m.zoomed = zoomed
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("View panicked at %dx%d: %v", size[0], size[1], r)
							}
						}()
						_ = m.View()
					})
				}
			}
		}
	}
}

// TestAgentsSidebarBorders: at 128x45 the sidebar owns the last 30
// columns; every body line must end with the sidebar's right border, not
// the clip ellipsis a too-wide box leaves behind.
func TestAgentsSidebarBorders(t *testing.T) {
	m := agentsFixture(t, 128, 45, 5)
	m.tab = tabChat

	lines := strings.Split(ansiSeq.ReplaceAllString(m.View(), ""), "\n")
	if len(lines) != 45 {
		t.Fatalf("View() returned %d lines, want 45", len(lines))
	}
	// 1-based lines 3..44 are the body: the top row (line 1) and the
	// toast row (line 2, armed by the fixture's failed worker) come
	// first, the footer (line 45) last.
	borders := map[rune]bool{'│': true, '╮': true, '╯': true, '┐': true, '┘': true}
	for i, ln := range lines[2:44] {
		rs := []rune(ln)
		if last := rs[len(rs)-1]; !borders[last] {
			t.Errorf("line %d (1-based) ends %q, want a box border (│, ╮, ╯, ┐, ┘): %.40q…", i+3, last, ln)
		}
	}
}

// TestAgentsViewExactShape: the Agents screen must draw exactly h lines,
// no line wider than w. The top row and every body row (the screen
// itself, including the agent strip/sidebar and all panes) are padded to
// exactly w; the two chrome rows View() adds around the body keep their
// existing widths — the toast row is left-padded to w-2 and the footer
// lands at w-1 (or w when the key hints clip it), per view.go's current
// behaviour, so the test pins those instead of failing on them.
func TestAgentsViewExactShape(t *testing.T) {
	type shapeCase struct {
		w, h     int
		nWorkers int
		focus    int
		zoomed   bool
	}
	var cases []shapeCase
	for _, nWorkers := range []int{0, 5} {
		for _, size := range [][2]int{{128, 45}, {160, 50}, {110, 35}, {90, 30}} {
			for _, focus := range []int{focusSup, focusWork, focusBottom} {
				for _, zoomed := range []bool{false, true} {
					cases = append(cases, shapeCase{
						w: size[0], h: size[1],
						nWorkers: nWorkers, focus: focus, zoomed: zoomed,
					})
				}
			}
		}
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%dworkers/%dx%d/focus=%s/zoomed=%v",
			tc.nWorkers, tc.w, tc.h,
			map[int]string{focusSup: "sup", focusWork: "work", focusBottom: "bottom"}[tc.focus],
			tc.zoomed)
		t.Run(name, func(t *testing.T) {
			m := agentsFixture(t, tc.w, tc.h, tc.nWorkers)
			m.tab = tabChat
			m.focus = tc.focus
			m.zoomed = tc.zoomed

			out := m.View()
			lines := strings.Split(out, "\n")
			if len(lines) != tc.h {
				t.Fatalf("View() returned %d lines, want %d\nout:\n%s", len(lines), tc.h, out)
			}

			// Top row: exactly w.
			if w := lipgloss.Width(lines[0]); w != tc.w {
				t.Errorf("top row width %d, want %d: %.80q", w, tc.w, lines[0])
			}

			// Body: everything between the top row (and the toast row,
			// when one is armed by the failed worker) and the footer.
			bodyStart := 1
			if m.toast != "" {
				bodyStart++
			}
			for i := bodyStart; i < len(lines)-1; i++ {
				if w := lipgloss.Width(lines[i]); w != tc.w {
					t.Errorf("body line %d width %d, want %d: %.80q", i, w, tc.w, lines[i])
				}
			}

			// Chrome rows: never overflow, and stay at their known widths.
			footer := lines[len(lines)-1]
			if w := lipgloss.Width(footer); w < tc.w-1 || w > tc.w {
				t.Errorf("footer width %d outside [%d,%d]: %.80q", w, tc.w-1, tc.w, footer)
			}
			if bodyStart > 1 {
				if w := lipgloss.Width(lines[1]); w > tc.w {
					t.Errorf("toast row width %d overflows w=%d: %.80q", w, tc.w, lines[1])
				}
			}
		})
	}
}
