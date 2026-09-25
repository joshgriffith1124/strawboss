package main

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// A file reference: a path token immediately followed by ":<line>".
	// The leading character (start of line, whitespace or "(") anchors the
	// match so mid-token digits like version numbers do not count.
	failFileRe = regexp.MustCompile(`(?m)(?:^|[\s(])((?:[\w.-]+/)*[\w.-]+\.[A-Za-z]\w*):\d+`)
	// A go build package line: "# example.com/m/internal/ui".
	failPkgRe = regexp.MustCompile(`^#\s+(\S+)`)
	// A go test package line: "FAIL\tpkg\tduration" / "FAIL  pkg [build failed]".
	// A bare "FAIL" line (no token) is not a package.
	failFailRe = regexp.MustCompile(`^FAIL[ \t]+(\S+)`)
)

// failRefs extracts what a failing check's output points at: file paths
// ("internal/ui/foo.go:12:3: undefined", "    foo_test.go:42: got 1",
// "./x.go:3:") and Go package import paths ("# example.com/m/internal/ui",
// "FAIL\texample.com/m/internal/live\t0.3s", "FAIL\texample.com/m/pkg [build failed]").
// Paths are returned with any leading "./" stripped, deduped, in order.
func failRefs(output string) (files, pkgs []string) {
	seen := map[string]bool{}
	add := func(list *[]string, s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		*list = append(*list, s)
	}
	for _, m := range failFileRe.FindAllStringSubmatch(output, -1) {
		add(&files, strings.TrimPrefix(m[1], "./"))
	}
	for _, line := range strings.Split(output, "\n") {
		if m := failPkgRe.FindStringSubmatch(line); m != nil && strings.Contains(m[1], "/") {
			add(&pkgs, m[1])
			continue
		}
		if m := failFailRe.FindStringSubmatch(line); m != nil && strings.Contains(m[1], "/") {
			add(&pkgs, m[1])
		}
	}
	return files, pkgs
}

// blame attributes a failed shared-directory check to workers. changed[i]
// is worker i's touched files (repo-relative; nil = unknown), tasks[i] its
// task text. A worker is implicated when one of its files matches a failing
// file (equal, or the changed path ends with "/"+ref — go test prints bare
// base names); only when no worker matches a file does a package match
// count (a changed file whose directory d satisfies pkg == d or
// strings.HasSuffix(pkg, "/"+d)). Workers sharing a directory see each
// other's edits in their snapshots, so a file matched by several workers is
// kept only for those whose task mentions it (mentionsAny in stray.go) —
// all of them when none does. culprits maps worker index -> the matched
// refs (files, or package dirs for package-level matches), sorted.
// ok is false when nothing could be attributed.
func blame(output string, changed [][]string, tasks []string) (map[int][]string, bool) {
	files, pkgs := failRefs(output)

	hits := map[int]map[string]bool{}
	addRef := func(i int, ref string) {
		if hits[i] == nil {
			hits[i] = map[string]bool{}
		}
		hits[i][ref] = true
	}

	fileHit := false
	for _, ref := range files {
		var hit []int
		for i, paths := range changed {
			for _, p := range paths {
				if p == ref || strings.HasSuffix(p, "/"+ref) {
					hit = append(hit, i)
					break
				}
			}
		}
		if len(hit) == 0 {
			continue
		}
		fileHit = true
		if len(hit) > 1 {
			keep := []int{}
			for _, i := range hit {
				if i < len(tasks) && mentionsAny(ref, tasks[i:i+1]) {
					keep = append(keep, i)
				}
			}
			if len(keep) > 0 {
				hit = keep
			}
		}
		for _, i := range hit {
			addRef(i, ref)
		}
	}

	if !fileHit {
		for _, pkg := range pkgs {
			for i, paths := range changed {
				for _, p := range paths {
					d := filepath.Dir(p)
					if d == pkg || strings.HasSuffix(pkg, "/"+d) {
						addRef(i, d)
					}
				}
			}
		}
	}

	if len(hits) == 0 {
		return nil, false
	}
	culprits := make(map[int][]string, len(hits))
	for i, refs := range hits {
		list := make([]string, 0, len(refs))
		for ref := range refs {
			list = append(list, ref)
		}
		sort.Strings(list)
		culprits[i] = list
	}
	return culprits, true
}

// blameNote is the line that replaces "(after all N workers — it cannot
// tell which one broke it)" when blame succeeds, e.g.
// "(points at w12: internal/ui/foo.go; w14: internal/live — not implicated: w11, w13)"
// ids[i] is worker i's id; at most 3 refs per culprit, then "+N more".
// The " — not implicated: ..." part is omitted when every worker is a culprit.
func blameNote(culprits map[int][]string, ids []string) string {
	var parts, credited []string
	for i, id := range ids {
		refs, ok := culprits[i]
		if !ok {
			credited = append(credited, id)
			continue
		}
		s := strings.Join(refs, ", ")
		if len(refs) > 3 {
			s = strings.Join(refs[:3], ", ") + " +" + strconv.Itoa(len(refs)-3) + " more"
		}
		parts = append(parts, id+": "+s)
	}
	note := "(points at " + strings.Join(parts, "; ")
	if len(credited) > 0 {
		note += " — not implicated: " + strings.Join(credited, ", ")
	}
	return note + ")"
}
