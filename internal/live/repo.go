package live

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// repoPollEvery is how often the repo probe runs. Git against a local
// worktree is cheap and branch moves are rare; polling avoids a
// file-notification dependency, like the models.toml watcher.
var repoPollEvery = 5 * time.Second

// repoProbeTimeout bounds each git call: a wedged git must not hold the
// probe — or the feed — hostage.
const repoProbeTimeout = 3 * time.Second

// repoInfo is one probe's parsed state. Comparable on purpose: the poller
// detects change with ==, so nothing here may be a slice or map.
type repoInfo struct {
	repo, branch string
	dirty        bool
}

// parseRepoInfo turns raw git output into display state. toplevel is `git
// rev-parse --show-toplevel`, abbrevRef `--abbrev-ref HEAD`, short
// `--short HEAD` (used only for a detached head, where abbrevRef prints
// "HEAD"), porcelain `status --porcelain`. Pure: no git, no I/O — this is
// the parsing test seam.
func parseRepoInfo(toplevel, abbrevRef, short, porcelain string) repoInfo {
	branch := strings.TrimSpace(abbrevRef)
	if branch == "HEAD" {
		// Detached: name the commit instead of the non-branch "HEAD".
		branch = strings.TrimSpace(short)
	}
	return repoInfo{
		repo:   filepath.Base(strings.TrimSpace(toplevel)),
		branch: branch,
		dirty:  strings.TrimSpace(porcelain) != "",
	}
}

// gitOut runs one git command in dir and returns its trimmed stdout.
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), repoProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

// gitRepoInfo probes the repo state in dir. ok=false when dir is not
// inside a work tree, git is missing, or git fails — the poller then
// emits nothing and keeps trying.
func gitRepoInfo(dir string) (repoInfo, bool) {
	toplevel, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return repoInfo{}, false
	}
	// No commits yet (or the worktree vanished mid-probe): abbrev-ref
	// fails, and there is no branch worth naming — wait for a real one
	// instead of rendering a dangling "name ⎇ " in the header.
	abbrevRef, err := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return repoInfo{}, false
	}
	short, _ := gitOut(dir, "rev-parse", "--short", "HEAD")
	porcelain, _ := gitOut(dir, "status", "--porcelain")
	return parseRepoInfo(toplevel, abbrevRef, short, porcelain), true
}

// pollRepo probes the repo every repoPollEvery and emits RepoInfoMsg into
// the feed when the state changes — and once at start, so the header
// shows it without waiting a full interval. Not a git repo (or git
// missing) emits nothing and keeps polling: the header just stays empty.
// All git runs here, never in the UI.
func (o *Orchestrator) pollRepo(ctx context.Context) {
	var last repoInfo
	tick := time.NewTicker(repoPollEvery)
	defer tick.Stop()
	probe := func() {
		cur, ok := gitRepoInfo(o.repoDir())
		if !ok || cur == last {
			return
		}
		last = cur
		o.emit(ctx, ui.RepoInfoMsg{Repo: cur.repo, Branch: cur.branch, Dirty: cur.dirty})
	}
	probe() // once at start
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			probe()
		}
	}
}
