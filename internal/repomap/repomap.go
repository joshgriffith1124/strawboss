// Package repomap builds a compact map of a repository — file paths with
// their top-level symbols — for injection into WORKER prompts. Small
// local models fail mostly from blindness: without repo awareness they
// invent file layouts. The map costs zero supervisor tokens (the harness
// side assembles it; it never appears in a terse result).
package repomap

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultBudget bounds the rendered map in bytes (~1.5k tokens): big
// enough to orient a worker, small enough to leave a dsh worker's ~48k
// context to the task.
const DefaultBudget = 6 * 1024

// maxFileBytes skips huge files during symbol extraction.
const maxFileBytes = 256 << 10

// symbolRes maps file extensions to top-level declaration matchers; the
// first capture group is the symbol name. Regex over tree-sitter on
// purpose: cheap first version — measure worker first-pass success
// before buying real parsing.
var symbolRes = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`^func (?:\([^)]+\) )?([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`^type ([A-Za-z_][A-Za-z0-9_]*)`),
	},
	".py": {
		regexp.MustCompile(`^(?:async )?def ([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`^class ([A-Za-z_][A-Za-z0-9_]*)`),
	},
	".js": jsRes, ".jsx": jsRes, ".ts": jsRes, ".tsx": jsRes, ".mjs": jsRes,
	".rs": {
		regexp.MustCompile(`^(?:pub(?:\([^)]*\))? )?(?:async )?fn ([A-Za-z_][A-Za-z0-9_]*)`),
		regexp.MustCompile(`^(?:pub(?:\([^)]*\))? )?(?:struct|enum|trait) ([A-Za-z_][A-Za-z0-9_]*)`),
	},
	".rb": {
		regexp.MustCompile(`^\s*def ([a-z_][A-Za-z0-9_?!]*)`),
		regexp.MustCompile(`^\s*(?:class|module) ([A-Z][A-Za-z0-9_:]*)`),
	},
}

var jsRes = []*regexp.Regexp{
	regexp.MustCompile(`^(?:export )?(?:default )?(?:async )?function\*? ([A-Za-z_$][A-Za-z0-9_$]*)`),
	regexp.MustCompile(`^(?:export )?(?:abstract )?class ([A-Za-z_$][A-Za-z0-9_$]*)`),
	regexp.MustCompile(`^(?:export )?(?:const|let) ([A-Za-z_$][A-Za-z0-9_$]*) =`),
}

const maxSymbolsPerFile = 12

// Build renders the map for dir, capped at budget bytes (DefaultBudget
// when budget <= 0). Returns "" when dir isn't a git repository or has
// no tracked files — the caller then injects nothing.
func Build(dir string, budget int) string {
	if budget <= 0 {
		budget = DefaultBudget
	}
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	files := strings.Fields(strings.TrimSpace(string(out)))
	if len(files) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Repository map (tracked files; top-level symbols where parsed):\n")
	skipped := 0
	for _, f := range files {
		line := "  " + f
		if syms := symbols(filepath.Join(dir, f), filepath.Ext(f)); len(syms) > 0 {
			line += ": " + strings.Join(syms, ", ")
		}
		if b.Len()+len(line)+1 > budget {
			skipped = len(files) - indexOf(files, f)
			break
		}
		b.WriteString(line + "\n")
	}
	if skipped > 0 {
		b.WriteString(fmt.Sprintf("  … +%d more files\n", skipped))
	}
	return b.String()
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return len(ss)
}

// symbols extracts up to maxSymbolsPerFile top-level declaration names.
func symbols(path, ext string) []string {
	res, ok := symbolRes[ext]
	if !ok {
		return nil
	}
	if info, err := os.Stat(path); err != nil || info.Size() > maxFileBytes {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() && len(out) < maxSymbolsPerFile {
		line := sc.Text()
		for _, re := range res {
			if m := re.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
				break
			}
		}
	}
	if len(out) == maxSymbolsPerFile {
		out[maxSymbolsPerFile-1] = "…"
	}
	return out
}

// Prompt composes the worker prompt: map first (context), task last
// (instruction) — the order small models follow best. An empty map
// returns the task unchanged.
func Prompt(repoMap, task string) string {
	if repoMap == "" {
		return task
	}
	return repoMap + "\nYour task:\n" + task
}
