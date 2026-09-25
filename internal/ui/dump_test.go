package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// agentsDumpState builds the same realistic Agents state at any size:
// an identified supervisor, a short chat (user prompt, reply, a
// delegation out, its result, and a failed one), six workers across
// qwen-dsh and fast-local with mixed statuses, transcript events per
// worker, and one stat row per model.
func agentsDumpState(t *testing.T, w, h int) Model {
	t.Helper()
	base := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	msgs := []tea.Msg{
		tea.WindowSizeMsg{Width: w, Height: h},
		SupInitMsg{SessionID: "0123456789abcdef", Model: "claude-fable-5", Auth: "subscription", PID: 41772},
		SupRateLimitMsg{FiveHour: 0.04, SevenDay: 0.11},
		SupUserMsg{Text: "Build the trade query builder module.", Time: base},
		SupTextDoneMsg{Text: "Splitting this into four tasks across two local models.", Time: base.Add(time.Second)},
		SupToolMsg{ToolID: "t1", Name: "delegate", Delegate: &DelegateInfo{Model: "qwen-dsh", Task: "scaffold the trade query builder"}},
		SupToolResultMsg{ToolID: "t1", Content: "w1 done 3m12s · log ~/.strawboss/logs/w1.log\nscaffold complete: 4 files, tests pass"},
		SupToolMsg{ToolID: "t2", Name: "delegate", Delegate: &DelegateInfo{Model: "fast-local", Task: "lint and format the new package"}},
		SupToolResultMsg{ToolID: "t2", Content: "w2 failed 1m04s · log ~/.strawboss/logs/w2.log\nlint: 2 errors in query_test.go", IsError: true},
		SupUsageMsg{Input: 1200, Output: 400, CacheRead: 14100, CacheWrite: 7200, CostUSD: 0.17, Turns: 1, CtxWindow: 200_000},
		WorkerUpsertMsg{ID: "w1", Model: "qwen-dsh", Task: "scaffold the trade query builder", Status: "done",
			Started: base, Ended: base.Add(3*time.Minute + 12*time.Second), Summary: "scaffold complete: 4 files", LogPath: "~/.strawboss/logs/w1.log"},
		WorkerUpsertMsg{ID: "w2", Model: "fast-local", Task: "lint and format the new package", Status: "failed",
			Started: base.Add(time.Minute), Ended: base.Add(time.Minute + 4*time.Second), Summary: "lint: 2 errors", LogPath: "~/.strawboss/logs/w2.log"},
		WorkerUpsertMsg{ID: "w3", Model: "qwen-dsh", Task: "write the integration tests", Status: "running",
			Started: base.Add(2 * time.Minute), LogPath: "~/.strawboss/logs/w3.log"},
		WorkerUpsertMsg{ID: "w4", Model: "fast-local", Task: "add docstrings to the public API", Status: "queued",
			Started: base.Add(3 * time.Minute), LogPath: "~/.strawboss/logs/w4.log"},
		WorkerUpsertMsg{ID: "w5", Model: "qwen-dsh", Task: "refactor the query builder core", Status: "running",
			Started: base.Add(4 * time.Minute), LogPath: "~/.strawboss/logs/w5.log"},
		WorkerUpsertMsg{ID: "w6", Model: "fast-local", Task: "update the README", Status: "done",
			Started: base.Add(5 * time.Minute), Ended: base.Add(7 * time.Minute), Summary: "README updated", LogPath: "~/.strawboss/logs/w6.log"},
		WorkerUsageMsg{ID: "w1", Input: 41000, Output: 9200},
		WorkerUsageMsg{ID: "w2", Input: 6000, Output: 1100},
		WorkerUsageMsg{ID: "w3", Input: 22000, Output: 4100, Ctx: 26000},
		WorkerUsageMsg{ID: "w5", Input: 31000, Output: 5400, Ctx: 40000},
		WorkerUsageMsg{ID: "w6", Input: 9000, Output: 1800},
		WorkerEventMsg{ID: "w1", Kind: "text", Text: "Scaffolding the trade package layout."},
		WorkerEventMsg{ID: "w1", Kind: "tool", Text: "bash go build ./... [completed]"},
		WorkerEventMsg{ID: "w1", Kind: "tool", Text: "bash go test ./trade/... [completed]"},
		WorkerEventMsg{ID: "w2", Kind: "tool", Text: "bash golangci-lint run ./trade/..."},
		WorkerEventMsg{ID: "w2", Kind: "error", Text: "lint: 2 errors in query_test.go"},
		WorkerEventMsg{ID: "w3", Kind: "text", Text: "Writing table-driven tests for the builder."},
		WorkerEventMsg{ID: "w3", Kind: "reasoning", Text: "Need to pin the plan-equiv split before asserting."},
		WorkerEventMsg{ID: "w3", Kind: "tool", Text: "bash go test -run TestQuery ./trade/..."},
		WorkerEventMsg{ID: "w4", Kind: "text", Text: "Queued behind w5 on the local endpoint."},
		WorkerEventMsg{ID: "w4", Kind: "reasoning", Text: "Will read the exported API surface first."},
		WorkerEventMsg{ID: "w5", Kind: "text", Text: "Extracting the plan parser from the builder."},
		WorkerEventMsg{ID: "w5", Kind: "tool", Text: "Read trade/plan.go"},
		WorkerEventMsg{ID: "w5", Kind: "tool", Text: "Edit trade/builder.go"},
		WorkerEventMsg{ID: "w6", Kind: "tool", Text: "Read README.md"},
		WorkerEventMsg{ID: "w6", Kind: "tool", Text: "Edit README.md [completed]"},
		ModelStatMsg{Name: "qwen-dsh", TokSec: 74, Active: 2, Note: "dsh · local"},
		ModelStatMsg{Name: "fast-local", TokSec: 120, Active: 1, Queue: 1, Note: "vllm · local"},
	}
	return apply(t, New(make(chan tea.Msg)), msgs...)
}

// TestDumpAgents renders the six tabs into files under the directory
// named by STRAWBOSS_DUMP (ANSI escapes stripped) so layout changes can
// be eyeballed against docs/MOCKUP.html. Skipped unless the variable
// points at a directory; it never asserts on the pixels.
func TestDumpAgents(t *testing.T) {
	dump := os.Getenv("STRAWBOSS_DUMP")
	if dump == "" {
		t.Skip("STRAWBOSS_DUMP not set")
	}
	if err := os.MkdirAll(dump, 0o755); err != nil {
		t.Fatalf("create dump dir %s: %v", dump, err)
	}

	write := func(name string, m Model) {
		t.Helper()
		out := ansiSeq.ReplaceAllString(m.View(), "")
		if err := os.WriteFile(filepath.Join(dump, name), []byte(out), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	for _, size := range [][2]int{{128, 45}, {160, 50}, {110, 35}, {90, 30}} {
		m := agentsDumpState(t, size[0], size[1])
		m.tab = tabChat
		write(fmt.Sprintf("agents-%dx%d.txt", size[0], size[1]), m)
	}

	m := agentsDumpState(t, 128, 45)
	m.tab = tabChat
	m.focus = focusWork
	write("agents-128x45-work.txt", m)

	m = agentsDumpState(t, 128, 45)
	m.tab = tabChat
	m.zoomed = true
	write("agents-128x45-zoom.txt", m)

	// Tasks tab: same rich state, a running worker selected so the live
	// pane and the DETAIL column have content.
	m = agentsDumpState(t, 128, 45)
	m.tab = tabDashboard
	for i, r := range m.visibleWorkers() {
		if r.ID == "w3" {
			m.selected = i
		}
	}
	write("tasks-128x45.txt", m)

	// Worktrees demo (no branch hook).
	m = New(make(chan tea.Msg))
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")},
	)
	write("worktrees-demo.txt", m)

	// Worktrees with branches + diff open.
	m = New(make(chan tea.Msg))
	now := time.Now()
	m.OnListBranches = func() []BranchInfo {
		return []BranchInfo{
			{Name: "strawboss/w1-trade-query", Ahead: 2, Files: 3, Added: 120, Deleted: 40,
				Subject: "scaffold trade query", When: now.Add(-5 * time.Minute)},
			{Name: "strawboss/w2-docstrings", Ahead: 1, Files: 1, Added: 30, Deleted: 2,
				Subject: "docstrings for pkg", When: now.Add(-time.Hour)},
		}
	}
	m.OnBranchDiff = func(name string) (string, error) {
		return "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n-old line\n+new line for " + name, nil
	}
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 100, Height: 30},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")},
		tea.KeyMsg{Type: tea.KeyDown},
		tea.KeyMsg{Type: tea.KeyEnter},
	)
	write("worktrees-diff.txt", m)

	// Models + Analytics tabs.
	m = demoState(t)
	m.OnScorecard = func() []ScoreRow {
		return []ScoreRow{
			{Model: "qwen-coder", Attempts: 10, Done: 8, Failed: 1, Rescued: 1, Median: 62 * time.Second},
			{Model: "fast-local", Attempts: 4, Done: 4, Failed: 0, Median: 45 * time.Second},
		}
	}
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 128, Height: 40},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("5")},
	)
	write("analytics-128x40.txt", m)

	m = demoState(t)
	m = apply(t, m,
		tea.WindowSizeMsg{Width: 128, Height: 40},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")},
	)
	write("models-128x40.txt", m)
}

// TestViewWidthDiscipline enforces the layout contract on every tab:
// every line of View() is exactly w cells wide at several widths.
func TestViewWidthDiscipline(t *testing.T) {
	for _, w := range []int{132, 110, 90, 70, 55, 40} {
		m := demoState(t)
		m = apply(t, m, tea.WindowSizeMsg{Width: w, Height: 24})
		for tab := 0; tab < numTabs; tab++ {
			m.tab = tab
			if tab == tabWorktrees && m.OnListBranches == nil {
				m.reviewing = true
			}
			v := ansiSeq.ReplaceAllString(m.View(), "")
			lines := strings.Split(v, "\n")
			for i, ln := range lines {
				if got := lipgloss.Width(ln); got != w {
					t.Errorf("w=%d tab=%d line %d: width %d: %.60q", w, tab, i, got, ln)
				}
			}
		}
	}
}
