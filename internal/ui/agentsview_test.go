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

// TestAgentsTilesHidden: ctrl+t hides the worker tiles on the Agents
// screen. The focused tile is always asserted on model state (focus plus
// selected worker id), never on border colour.
func TestAgentsTilesHidden(t *testing.T) {
	base := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)

	t.Run("ctrl+t toggles from the supervisor focus", func(t *testing.T) {
		m := agentsFixture(t, 132, 40, 5)
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if !m.hideTiles {
			t.Fatal("ctrl+t did not hide the tiles")
		}
		if m.focus != focusSup {
			t.Fatalf("focus = %d, want focusSup after toggling from the supervisor", m.focus)
		}
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if m.hideTiles {
			t.Fatal("second ctrl+t did not reveal the tiles")
		}
	})

	t.Run("ctrl+t hands a focused tile's keyboard back to the supervisor", func(t *testing.T) {
		m := agentsFixture(t, 132, 40, 5)
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab}) // w1's tile
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab}) // w2's tile
		if m.focus != focusWork || m.selectedWorker() == nil || m.selectedWorker().ID != "w2" {
			t.Fatalf("focus=%d sel=%+v, want focusWork/w2 before hiding", m.focus, m.selectedWorker())
		}
		m.zoomed = true
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if !m.hideTiles || m.focus != focusSup || m.zoomed {
			t.Fatalf("hideTiles=%v focus=%d zoomed=%v, want hidden/focusSup/unzoomed",
				m.hideTiles, m.focus, m.zoomed)
		}
	})

	t.Run("ctrl+t only toggles on the Agents screen", func(t *testing.T) {
		m := agentsFixture(t, 132, 40, 5)
		m.tab = tabDashboard
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if m.hideTiles {
			t.Fatal("ctrl+t hid the tiles off the Agents screen")
		}
	})

	t.Run("cyclePane skips hidden tiles", func(t *testing.T) {
		m := agentsFixture(t, 132, 40, 5)
		// Tiles shown: the cycle reaches the first active worker tile.
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.focus != focusWork || m.selectedWorker() == nil || m.selectedWorker().ID != "w1" {
			t.Fatalf("focus=%d sel=%+v, want focusWork/w1 with tiles shown",
				m.focus, m.selectedWorker())
		}
		// Hide the tiles (hands off to the supervisor), then cycle: the
		// hidden tiles must not be focus stops.
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.focus != focusBottom {
			t.Fatalf("focus = %d, want focusBottom: hidden tiles are not focus stops", m.focus)
		}
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.focus != focusSup {
			t.Fatalf("focus = %d, want focusSup after the bottom panel", m.focus)
		}
		// Reveal the tiles: the cycle reaches the worker tiles again.
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.focus != focusWork || m.selectedWorker() == nil || m.selectedWorker().ID != "w1" {
			t.Fatalf("focus=%d sel=%+v, want focusWork/w1 with tiles revealed",
				m.focus, m.selectedWorker())
		}
	})

	t.Run("tiles hidden label appears in the view", func(t *testing.T) {
		m := agentsFixture(t, 128, 45, 5)
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if !strings.Contains(m.View(), "tiles hidden") {
			t.Fatal(`View() missing "tiles hidden" while the tiles are tucked away`)
		}
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		if strings.Contains(m.View(), "tiles hidden") {
			t.Fatal(`View() still says "tiles hidden" after the tiles are revealed`)
		}
	})

	t.Run("tiles hidden keeps the exact frame", func(t *testing.T) {
		m := agentsFixture(t, 128, 45, 5)
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyCtrlT})
		out := m.View()
		lines := strings.Split(out, "\n")
		if len(lines) != 45 {
			t.Fatalf("View() returned %d lines, want 45\nout:\n%s", len(lines), out)
		}
		if w := lipgloss.Width(lines[0]); w != 128 {
			t.Errorf("top row width %d, want 128: %.80q", w, lines[0])
		}
		bodyStart := 1
		if m.toast != "" {
			bodyStart++
		}
		for i := bodyStart; i < len(lines)-1; i++ {
			if w := lipgloss.Width(lines[i]); w != 128 {
				t.Errorf("body line %d width %d, want 128: %.80q", i, w, lines[i])
			}
		}
	})

	t.Run("selection stays on the same worker id when the order shifts", func(t *testing.T) {
		m := agentsFixture(t, 132, 40, 5)
		// Live rows: queued first, then running in spawn order — w1 sits
		// at visible index 1, below the queued w2.
		m = agentsFeed(t, m, tea.KeyMsg{Type: tea.KeyShiftTab}) // w1's tile
		if m.focus != focusWork || m.selectedWorker() == nil || m.selectedWorker().ID != "w1" {
			t.Fatalf("focus=%d sel=%+v, want focusWork/w1", m.focus, m.selectedWorker())
		}
		if m.selIndex(m.visibleWorkers()) != 1 {
			t.Fatalf("w1 at row %d, want 1 before the order shifts", m.selIndex(m.visibleWorkers()))
		}
		// w1 finishes: it drops out of the live rows and sinks into the
		// finished history, so its row number shifts.
		m = agentsFeed(t, m,
			WorkerUpsertMsg{ID: "w1", Status: "done", Started: base, Ended: base.Add(10 * time.Minute)})
		got := m.selIndex(m.visibleWorkers())
		if m.selectedWorker() == nil || m.selectedWorker().ID != "w1" {
			t.Fatalf("sel=%+v, want w1: a bare index would have drifted off", m.selectedWorker())
		}
		if got == 1 {
			t.Fatal("w1 did not move: the display order did not shift, so the test would not prove anything")
		}
		if m.visibleWorkers()[got].ID != "w1" {
			t.Fatalf("row %d holds %s, want w1", got, m.visibleWorkers()[got].ID)
		}
	})
}

// TestSupCtxLabel pins the supervisor context gauge in the Agents pane
// header: empty before any usage, footprint-only while the window is
// unknown, nearest-percent share once known, and the /new nudge past the
// bloat line.
func TestSupCtxLabel(t *testing.T) {
	tests := []struct {
		name  string
		ctx   int
		win   int
		empty bool
		want  []string
		lacks []string
	}{
		{name: "no context yet", ctx: 0, empty: true},
		{
			name:  "unknown window shows footprint only",
			ctx:   168000,
			want:  []string{"ctx 168"},
			lacks: []string{"%"},
		},
		{
			name:  "known window under the bloat line",
			ctx:   168000,
			win:   1000000,
			want:  []string{"17%"},
			lacks: []string{"/new"},
		},
		{
			name: "bloated window suggests /new",
			ctx:  600000,
			win:  1000000,
			want: []string{"/new"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil)
			m.supCtx = tc.ctx
			m.supCtxWindow = tc.win
			got := m.supCtxLabel()
			if tc.empty {
				if got != "" {
					t.Fatalf("supCtxLabel() = %q, want empty", got)
				}
				return
			}
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("supCtxLabel() = %q, want it to contain %q", got, s)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(got, s) {
					t.Errorf("supCtxLabel() = %q, want it to not contain %q", got, s)
				}
			}
		})
	}
}

// TestSupCtxLabelInView: at 128x45 the gauge must land in the Agents
// screen's supervisor pane, and the frame must stay exactly 45 lines of
// 128 cells each.
func TestSupCtxLabelInView(t *testing.T) {
	m := agentsFixture(t, 128, 45, 0)
	m.supCtx = 168000
	m.supCtxWindow = 1000000
	m.tab = tabChat

	out := m.View()
	if !strings.Contains(out, "ctx 168") {
		t.Fatalf("View() missing the context gauge:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 45 {
		t.Fatalf("View() returned %d lines, want 45", len(lines))
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w != 128 {
			t.Errorf("line %d width %d, want 128: %.80q", i+1, w, ln)
		}
	}
}
