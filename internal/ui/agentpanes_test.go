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

// TestRepoLabel: empty before the first RepoInfoMsg, then "repo ⎇ branch"
// with a " *" suffix when the worktree is dirty; short on room, the repo
// name drops before the branch is cut.
func TestRepoLabel(t *testing.T) {
	m := New(nil)
	if got := m.repoLabel(80); got != "" {
		t.Fatalf("fresh model repoLabel = %q, want empty", got)
	}
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane"})
	if got := m.repoLabel(80); got != "strawboss ⎇ repo-info-pane" {
		t.Fatalf("repoLabel = %q", got)
	}
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane", Dirty: true})
	tests := []struct {
		room int
		want string
	}{
		{80, "strawboss ⎇ repo-info-pane *"},
		{20, "⎇ repo-info-pane *"}, // repo name dropped, branch whole
	}
	for _, tt := range tests {
		if got := m.repoLabel(tt.room); got != tt.want {
			t.Errorf("repoLabel(%d) = %q, want %q", tt.room, got, tt.want)
		}
	}
	if got := m.repoLabel(10); lipgloss.Width(got) > 10 || !strings.HasPrefix(got, "⎇ repo") {
		t.Errorf("repoLabel(10) = %q, want the branch cut to fit", got)
	}
}

// TestSupHeaderBranchBeatsModel: with a long model name and the indicators
// on, the whole branch still shows — the model name yields first.
func TestSupHeaderBranchBeatsModel(t *testing.T) {
	m := agentsFixture(t, 128, 45, 0)
	m.supModel = "claude-opus-5-5[1m]-with-a-long-suffix"
	m.rolesOn = true
	m.supCtx, m.supCtxWindow = 168000, 1000000
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane", Dirty: true})
	m.tab = tabChat
	out := m.View()
	if !strings.Contains(out, "repo-info-pane *") {
		t.Errorf("branch cut off in the supervisor header:\n%s", strings.SplitN(out, "\n", 3)[1])
	}
	for i, ln := range strings.Split(out, "\n") {
		if w := lipgloss.Width(ln); w != 128 {
			t.Errorf("line %d width %d, want 128", i+1, w)
		}
	}
}

// TestSupRepoInfoInView: at 128x45 the repo and branch land in the Agents
// screen's supervisor pane header, and the frame stays exactly 45 lines of
// 128 cells each.
func TestSupRepoInfoInView(t *testing.T) {
	m := agentsFixture(t, 128, 45, 0)
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane", Dirty: true})
	m.tab = tabChat

	out := m.View()
	for _, want := range []string{"strawboss", "repo-info-pane"} {
		if !strings.Contains(out, want) {
			t.Fatalf("View() missing %q:\n%s", want, out)
		}
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

// TestSupRepoInfoNarrowPane: the label is the header's lowest priority —
// at a 1024x768-ish 100 columns it renders and every line still fits, in
// a narrow pane it truncates to the room the right side left, and below a
// readable minimum it disappears instead of pushing the header out.
func TestSupRepoInfoNarrowPane(t *testing.T) {
	fit := func(t *testing.T, out string, w int) {
		t.Helper()
		for i, ln := range strings.Split(out, "\n") {
			if got := lipgloss.Width(ln); got != w {
				t.Errorf("line %d width %d, want %d: %.80q", i+1, got, w, ln)
			}
		}
	}

	// 100 columns: shown, and the frame stays exactly 100 cells wide.
	m := agentsFixture(t, 100, 30, 0)
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane", Dirty: true})
	m.tab = tabChat
	out := m.View()
	if !strings.Contains(out, "strawboss") || !strings.Contains(out, "repo-info-pane") {
		t.Fatalf("100-col View() missing the repo/branch:\n%s", out)
	}
	fit(t, out, 100)

	// Middle width: the branch outranks the model name — the whole branch
	// shows and the model name (also in the sidebar) yields its slot.
	m = agentsFixture(t, 60, 10, 0)
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane"})
	pane := m.viewSupPane(60, 10, true)
	plain := ansiSeq.ReplaceAllString(pane, "")
	if !strings.Contains(plain, "repo-info-pane") {
		t.Fatalf("60-col pane cut the branch: %q", plain)
	}
	if strings.Contains(plain, "claude-fable-5") {
		t.Fatalf("60-col pane kept the model name over the branch: %q", plain)
	}
	fit(t, pane, 60)

	// Tighter: the repo name drops before the branch is cut.
	m = agentsFixture(t, 44, 10, 0)
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane"})
	pane = m.viewSupPane(44, 10, true)
	plain = ansiSeq.ReplaceAllString(pane, "")
	if strings.Contains(plain, "strawboss ⎇") || !strings.Contains(plain, "⎇ repo") {
		t.Fatalf("44-col pane: want the branch without the repo name: %q", plain)
	}
	fit(t, pane, 44)

	// Very narrow: no room even for a readable prefix — gone entirely.
	m = agentsFixture(t, 30, 10, 0)
	m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane"})
	pane = m.viewSupPane(30, 10, true)
	plain = ansiSeq.ReplaceAllString(pane, "")
	if strings.Contains(plain, "repo-info-pane") {
		t.Fatalf("30-col pane shows the label without room: %q", plain)
	}
	fit(t, pane, 30)

	// Degenerate sizes must not panic with a label armed (the tiny-size
	// fixture covers View() without one).
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 3}} {
		m = agentsFixture(t, size[0], size[1], 0)
		m = agentsFeed(t, m, RepoInfoMsg{Repo: "strawboss", Branch: "repo-info-pane"})
		m.viewSupPane(size[0], size[1], true)
	}
}
