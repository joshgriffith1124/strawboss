package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// gitIn runs git in dir, failing the test on any error (tests shell out to
// git the way worktree_test does).
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestParseRepoInfo: the raw git output → display state seam. Everything
// here is strings in, struct out — no git, no I/O.
func TestParseRepoInfo(t *testing.T) {
	tests := []struct {
		name                       string
		toplevel, abbrevRef, short string
		porcelain                  string
		want                       repoInfo
	}{
		{"normal branch", "/home/j/strawboss\n", "repo-info-pane\n", "", "", repoInfo{"strawboss", "repo-info-pane", false}},
		{"detached head", "/tmp/x\n", "HEAD\n", "abc1234\n", "", repoInfo{"x", "abc1234", false}},
		{"dirty modified", "/home/j/strawboss\n", "main\n", "", " M main.go\n", repoInfo{"strawboss", "main", true}},
		{"dirty untracked", "/home/j/strawboss\n", "main\n", "", "?? new.txt\n", repoInfo{"strawboss", "main", true}},
		{"clean", "/home/j/strawboss\n", "main\n", "", "", repoInfo{"strawboss", "main", false}},
		{"toplevel trailing slash", "/home/j/strawboss/\n", "main", "", "", repoInfo{"strawboss", "main", false}},
		{"detached wins over short", "/tmp/x", "HEAD", "abc1234", " M f\n", repoInfo{"x", "abc1234", true}},
		{"padded inputs", "  /tmp/x  \n", "  main  \n", "", "\n", repoInfo{"x", "main", false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRepoInfo(tt.toplevel, tt.abbrevRef, tt.short, tt.porcelain)
			if got != tt.want {
				t.Errorf("parseRepoInfo(%q, %q, %q, %q) = %+v, want %+v",
					tt.toplevel, tt.abbrevRef, tt.short, tt.porcelain, got, tt.want)
			}
		})
	}
}

// TestGitRepoInfo probes a real temp repo: branch, dirty, detached, and
// the not-a-repo case.
func TestGitRepoInfo(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "t@t")
	gitIn(t, dir, "config", "user.name", "t")
	name := filepath.Base(dir)

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "init")

	info, ok := gitRepoInfo(dir)
	if !ok {
		t.Fatal("gitRepoInfo failed in a repo")
	}
	if info.repo != name {
		t.Errorf("repo = %q, want %q", info.repo, name)
	}
	if info.branch == "" || info.branch == "HEAD" {
		t.Errorf("branch = %q, want a real branch name", info.branch)
	}
	if info.dirty {
		t.Error("fresh commit reported dirty")
	}

	// An uncommitted file flips the dirty flag.
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, _ = gitRepoInfo(dir); !info.dirty {
		t.Error("untracked file not reported dirty")
	}

	// Detached head: the branch becomes the short commit, not "HEAD".
	gitIn(t, dir, "checkout", "--detach", "HEAD")
	info, ok = gitRepoInfo(dir)
	if !ok {
		t.Fatal("gitRepoInfo failed detached")
	}
	if short := gitIn(t, dir, "rev-parse", "--short", "HEAD"); info.branch != short {
		t.Errorf("detached branch = %q, want short commit %q", info.branch, short)
	}

	// Not a repo: ok=false, nothing to show.
	if _, ok := gitRepoInfo(t.TempDir()); ok {
		t.Error("empty dir reported as a repo")
	}

	// Repo with no commits yet: --show-toplevel succeeds but abbrev-ref
	// fails — nothing to show until the first commit exists.
	empty := t.TempDir()
	gitIn(t, empty, "init")
	if _, ok := gitRepoInfo(empty); ok {
		t.Error("commitless repo reported as a repo")
	}
}

// TestPollRepo: the poller emits once at start and again when the state
// changes, and keeps quiet in a non-repo.
func TestPollRepo(t *testing.T) {
	old := repoPollEvery
	repoPollEvery = 20 * time.Millisecond
	t.Cleanup(func() { repoPollEvery = old })

	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "t@t")
	gitIn(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "init")

	o := New(&supervisor.Driver{Dir: dir}, nil, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.pollRepo(ctx)

	msgs := drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		_, ok := m.(ui.RepoInfoMsg)
		return ok
	})
	first := msgs[len(msgs)-1].(ui.RepoInfoMsg)
	if first.Repo != filepath.Base(dir) || first.Branch == "" || first.Dirty {
		t.Fatalf("first probe = %+v", first)
	}

	// An uncommitted change is the next distinct state → one new msg.
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs = drainUntil(t, o.Feed(), 5*time.Second, func(m tea.Msg) bool {
		r, ok := m.(ui.RepoInfoMsg)
		return ok && r.Dirty
	})
	if last := msgs[len(msgs)-1].(ui.RepoInfoMsg); !last.Dirty || last.Repo != first.Repo {
		t.Fatalf("dirty probe = %+v", last)
	}
	cancel()

	// Not a repo: nothing is ever emitted. Bounded select, not drainUntil
	// (which fatals) — silence is the expected outcome.
	o = New(&supervisor.Driver{Dir: t.TempDir()}, nil, t.TempDir())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go o.pollRepo(ctx2)
	select {
	case m := <-o.Feed():
		if _, ok := m.(ui.RepoInfoMsg); ok {
			t.Fatalf("non-repo emitted %+v", m)
		}
	case <-time.After(200 * time.Millisecond):
	}
}
