package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// unionAdded returns the deduped union of two added-file lists, order
// preserved: all of a first, then b's entries not already in a. Both
// attempts of an escalated task share the working tree, so the union is
// the full set of files the task's run created.
func unionAdded(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, p := range a {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// strayFiles returns the paths in added that none of the tasks mention, so a
// worker's leftover debug files reach the supervisor. A path counts as
// mentioned when any task text contains the path itself, its base name, its
// parent directory (not "."), or — for a Go test file — the name of the file
// under test (foo_test.go is covered by "foo.go"). All tasks of the call are
// checked, since parallel workers in one directory see each other's files.
func strayFiles(added []string, tasks []string) []string {
	var stray []string
	for _, p := range added {
		if !mentionsAny(p, tasks) {
			stray = append(stray, p)
		}
	}
	return stray
}

// mentionsAny reports whether any task text mentions file p: the path
// itself, its base name, its parent directory (not "."), or — for a Go
// test file — the name of the file under test.
func mentionsAny(p string, tasks []string) bool {
	base := filepath.Base(p)
	candidates := []string{p, base}
	if d := filepath.Dir(p); d != "." {
		candidates = append(candidates, d)
	}
	if strings.HasSuffix(base, "_test.go") {
		candidates = append(candidates, strings.TrimSuffix(base, "_test.go"))
	}
	for _, c := range candidates {
		for _, t := range tasks {
			if strings.Contains(t, c) {
				return true
			}
		}
	}
	return false
}

// strayNote formats the result line for stray files: at most 5 names, then
// "and N more". Empty input returns "".
// e.g. "[new files not named in the task: a.go, b/c_test.go — delete them if they are leftovers]"
func strayNote(stray []string) string {
	if len(stray) == 0 {
		return ""
	}
	names := stray
	rest := ""
	if len(names) > 5 {
		names = names[:5]
		rest = ", and " + strconv.Itoa(len(stray)-5) + " more"
	}
	return "[new files not named in the task: " + strings.Join(names, ", ") + rest + " — delete them if they are leftovers]"
}
