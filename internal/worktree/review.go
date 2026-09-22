// Branch review is the missing last step of `delegate --worktree`. That
// flag parks each worker's finished work as commits on its own
// strawboss/* branch and stops there: nothing merges automatically, so
// the worker branches just accumulate in the repo until a human deals
// with them. These operations let the TUI list those awaiting branches,
// show what each one changed, and merge or discard one with a keystroke
// instead of a human dropping to git. The mutating operations refuse
// anything that does not carry the BranchPrefix, so the user's own
// branches can never be merged or deleted through here.
package worktree

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Branch is one worker branch awaiting review.
type Branch struct {
	Name     string    // full branch name, e.g. "strawboss/20260922-101500-abc-t1"
	Worktree string    // path of the worktree that has it checked out ("" if none)
	Ahead    int       // commits on the branch not on the repo's current HEAD
	Files    int       // files changed vs the merge base with HEAD
	Added    int       // lines added
	Deleted  int       // lines deleted
	Subject  string    // subject line of the branch tip commit
	When     time.Time // tip commit's committer date
}

// defaultDiffCap is the Diff cap used when the caller does not give one.
const defaultDiffCap = 256 * 1024

// owned reports whether branch is a strawboss worker branch this package
// is allowed to touch; everything else is refused.
func owned(branch string) error {
	if !strings.HasPrefix(branch, BranchPrefix) {
		return fmt.Errorf("refusing branch %q: only %q worker branches may be reviewed", branch, BranchPrefix)
	}
	return nil
}

// worktreesByBranch maps each branch checked out in a worktree of
// repoDir to that worktree's path.
func worktreesByBranch(repoDir string) (map[string]string, error) {
	out, err := git(repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}
	byBranch := map[string]string{}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
		} else if ref, ok := strings.CutPrefix(line, "branch "); ok {
			byBranch[strings.TrimPrefix(ref, "refs/heads/")] = path
		}
	}
	return byBranch, nil
}

// numstat counts the files, added lines and deleted lines of
// `git diff --numstat HEAD...branch`. Binary files show "-" for both line
// counts: they still count as a changed file, with no lines.
func numstat(repoDir, branch string) (files, added, deleted int, err error) {
	out, err := git(repoDir, "diff", "--numstat", "HEAD..."+branch)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("diffing %s: %w", branch, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		files++
		if v, aerr := strconv.Atoi(fields[0]); aerr == nil {
			added += v
		}
		if v, aerr := strconv.Atoi(fields[1]); aerr == nil {
			deleted += v
		}
	}
	return files, added, deleted, nil
}

// ListBranches returns every BranchPrefix branch in repoDir, newest tip
// first.
func ListBranches(repoDir string) ([]Branch, error) {
	out, err := git(repoDir, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)%09%(committerdate:unix)%09%(subject)",
		"refs/heads/"+BranchPrefix)
	if err != nil {
		return nil, fmt.Errorf("listing branches: %w", err)
	}
	worktrees, err := worktreesByBranch(repoDir)
	if err != nil {
		return nil, err
	}
	branches := []Branch{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			return nil, fmt.Errorf("listing branches: bad for-each-ref line %q", line)
		}
		ts, perr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("listing branches: bad committerdate in %q: %w", line, perr)
		}
		b := Branch{
			Name:     fields[0],
			Worktree: worktrees[fields[0]],
			When:     time.Unix(ts, 0).UTC(),
		}
		if len(fields) == 3 {
			b.Subject = fields[2]
		}
		count, err := git(repoDir, "rev-list", "--count", "HEAD.."+b.Name)
		if err != nil {
			return nil, fmt.Errorf("counting commits on %s: %w", b.Name, err)
		}
		b.Ahead, _ = strconv.Atoi(count)
		b.Files, b.Added, b.Deleted, err = numstat(repoDir, b.Name)
		if err != nil {
			return nil, err
		}
		branches = append(branches, b)
	}
	return branches, nil
}

// Diff returns `git diff HEAD...<branch>` (the branch's changes since it
// forked) prefixed by its --stat summary, capped at maxBytes (a trailing
// "\n… diff truncated (N bytes)" line when cut; maxBytes <= 0 means
// 256 KiB).
func Diff(repoDir, branch string, maxBytes int) (string, error) {
	if err := owned(branch); err != nil {
		return "", err
	}
	if maxBytes <= 0 {
		maxBytes = defaultDiffCap
	}
	stat, err := git(repoDir, "diff", "--stat", "HEAD..."+branch)
	if err != nil {
		return "", err
	}
	full, err := git(repoDir, "diff", "HEAD..."+branch)
	if err != nil {
		return "", err
	}
	out := stat
	if full != "" {
		out += "\n\n" + full
	}
	if len(out) > maxBytes {
		cut := out[:maxBytes]
		for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
			cut = cut[:len(cut)-1]
		}
		out = cut + fmt.Sprintf("\n… diff truncated (%d bytes)", len(out))
	}
	return out, nil
}

// Merge merges branch into repoDir's current branch with
// `git merge --no-ff --no-edit`, then removes its worktree and deletes the
// branch. It refuses (error, nothing changed) when repoDir has uncommitted
// changes to tracked files (`git status --porcelain --untracked-files=no`
// non-empty) or is in detached HEAD. On a conflict it runs
// `git merge --abort` and returns an error naming the conflicted files
// (from `git diff --name-only --diff-filter=U` before aborting) — the repo
// is left exactly as before.
func Merge(repoDir, branch string) error {
	if err := owned(branch); err != nil {
		return err
	}
	status, err := git(repoDir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("checking repo state before merge: %w", err)
	}
	if status != "" {
		return fmt.Errorf("merging %s: repo has uncommitted changes to tracked files", branch)
	}
	if _, err := git(repoDir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("merging %s: repo is in detached HEAD", branch)
	}
	if _, err := git(repoDir, "merge", "--no-ff", "--no-edit", branch); err != nil {
		conflicted, _ := git(repoDir, "diff", "--name-only", "--diff-filter=U")
		var files []string
		if conflicted != "" {
			files = strings.Split(conflicted, "\n")
		}
		if _, aerr := git(repoDir, "merge", "--abort"); aerr != nil {
			if len(files) > 0 {
				return fmt.Errorf("merging %s: conflict in %s, aborting the merge failed: %w",
					branch, strings.Join(files, ", "), aerr)
			}
			return fmt.Errorf("merging %s: %w", branch, err)
		}
		if len(files) > 0 {
			return fmt.Errorf("merging %s: conflict in: %s", branch, strings.Join(files, ", "))
		}
		return fmt.Errorf("merging %s: %w", branch, err)
	}
	worktrees, err := worktreesByBranch(repoDir)
	if err != nil {
		return err
	}
	if path, ok := worktrees[branch]; ok {
		return Remove(repoDir, path, branch)
	}
	if _, err := git(repoDir, "branch", "-d", branch); err != nil {
		return fmt.Errorf("deleting merged branch %s: %w", branch, err)
	}
	return nil
}

// Discard removes branch's worktree (if any) and deletes the branch, even
// if unmerged.
func Discard(repoDir, branch string) error {
	if err := owned(branch); err != nil {
		return err
	}
	worktrees, err := worktreesByBranch(repoDir)
	if err != nil {
		return err
	}
	if path, ok := worktrees[branch]; ok {
		return Remove(repoDir, path, branch)
	}
	if _, err := git(repoDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("deleting branch %s: %w", branch, err)
	}
	return nil
}
