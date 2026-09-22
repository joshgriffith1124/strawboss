package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Fingerprint summarizes the working tree's state in dir: HEAD, all
// tracked changes, and untracked files. Equal fingerprints mean the tree
// did not change in between. ok is false when dir is not inside a git
// work tree (or git fails) — callers must then assume it changed.
//
// The verify gate runs the project's check after a worker finishes. A
// worker that only read files or reported something changed nothing, and
// running a possibly minutes-long test suite after it is pure waste.
// Fingerprint lets the caller take a snapshot before the task and skip
// the check when the tree is unchanged afterwards.
func Fingerprint(dir string) (fp string, ok bool) {
	inside, err := gitOut(dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return "", false
	}

	// A worker that commits changes HEAD. A repo with no commits yet
	// fails rev-parse HEAD — that is a real tree state, hashed as the
	// literal "no-head" rather than a failure.
	head, err := gitOut(dir, "rev-parse", "HEAD")
	noHead := err != nil
	if noHead {
		head = "no-head"
	}

	h := sha256.New()
	fmt.Fprintf(h, "head\x00%s\n", strings.TrimSpace(head))

	// Tracked changes. With a HEAD, one diff covers staged + unstaged;
	// without, staged (against the empty tree) plus unstaged.
	if noHead {
		cached, err := gitOut(dir, "diff", "--cached", "--binary")
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "diff-cached\x00\n%s\n", cached)
		unstaged, err := gitOut(dir, "diff", "--binary")
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "diff-worktree\x00\n%s\n", unstaged)
	} else {
		diff, err := gitOut(dir, "diff", "HEAD", "--binary")
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "diff\x00\n%s\n", diff)
	}

	// Untracked, non-ignored files. Stat each instead of reading it: a
	// name/size/mtime change reveals an edit without touching content.
	untracked, err := gitOut(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", false
	}
	files := splitNULs(untracked)
	sort.Strings(files)
	for _, name := range files {
		li, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return "", false
		}
		fmt.Fprintf(h, "file\x00%s\x00size:%d\x00mtime:%d\n", name, li.Size(), li.ModTime().UnixNano())
	}

	return hex.EncodeToString(h.Sum(nil)), true
}

// gitOut runs git with cmd.Dir = dir and returns its stdout.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// splitNULs splits -z git output (NUL-terminated entries) into file names.
func splitNULs(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
}
