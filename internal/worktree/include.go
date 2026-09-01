package worktree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// IncludeFile is the ecosystem convention (shared with ccmanager, Claude
// Squad, Conductor) naming gitignored paths a fresh worktree needs to
// actually run: env files, local certs, untracked config.
const IncludeFile = ".worktreeinclude"

// CopyIncludes copies the paths repoDir's .worktreeinclude names into the
// worktree at dst. Each non-blank, non-# line is a glob relative to the
// repo root; directories copy recursively. Existing files in the worktree
// (tracked content) are never overwritten. No .worktreeinclude means
// nothing to do. Returns how many files were copied.
func CopyIncludes(repoDir, dst string) (int, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, IncludeFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", IncludeFile, err)
	}
	copied := 0
	for _, line := range strings.Split(string(data), "\n") {
		pat := strings.TrimSpace(line)
		if pat == "" || strings.HasPrefix(pat, "#") {
			continue
		}
		// Patterns must stay inside the repo: no absolute paths, no
		// parent escapes.
		if filepath.IsAbs(pat) || strings.Contains(pat, "..") {
			return copied, fmt.Errorf("%s: pattern %q escapes the repository", IncludeFile, pat)
		}
		matches, err := filepath.Glob(filepath.Join(repoDir, pat))
		if err != nil {
			return copied, fmt.Errorf("%s: pattern %q: %w", IncludeFile, pat, err)
		}
		for _, src := range matches {
			rel, err := filepath.Rel(repoDir, src)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			n, err := copyTree(src, filepath.Join(dst, rel))
			if err != nil {
				return copied, fmt.Errorf("%s: copying %s: %w", IncludeFile, rel, err)
			}
			copied += n
		}
	}
	return copied, nil
}

// copyTree copies a file, or a directory recursively, preserving file
// modes. Files already present at the destination are left untouched.
func copyTree(src, dst string) (int, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// A symlink's target may point anywhere; skip rather than copy
		// something the pattern never named.
		return 0, nil
	case info.IsDir():
		entries, err := os.ReadDir(src)
		if err != nil {
			return 0, err
		}
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return 0, err
		}
		total := 0
		for _, e := range entries {
			n, err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	default:
		if _, err := os.Lstat(dst); err == nil {
			return 0, nil // tracked content already checked out
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		in, err := os.Open(src)
		if err != nil {
			return 0, err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return 0, err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return 0, err
		}
		return 1, out.Close()
	}
}
