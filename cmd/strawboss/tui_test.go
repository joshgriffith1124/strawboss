package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSupervisorAllowedTools(t *testing.T) {
	// The baseline is asserted by content in
	// TestSupervisorAllowedToolsCoversRealDenials; this test is about the
	// append-only contract, so it takes the baseline as given.
	base := supervisorAllowedTools("/opt/strawboss", nil)
	tests := []struct {
		name  string
		extra []string
		want  []string
	}{
		{name: "no config extras", extra: nil, want: base},
		{
			name:  "config extras extend, never replace",
			extra: []string{"Bash(git status:*)", "Bash(git log:*)"},
			want:  append(append([]string{}, base...), "Bash(git status:*)", "Bash(git log:*)"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := supervisorAllowedTools("/opt/strawboss", tt.extra)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("supervisorAllowedTools() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSupervisorAllowedToolsCoversRealDenials: every entry here was denied
// in a real run (docs/NOTES.md, the transcript audit). Invariant 6 says
// the supervisor must never block on a permission prompt, so the
// allowlist has to cover what it is actually expected to do.
func TestSupervisorAllowedToolsCoversRealDenials(t *testing.T) {
	got := supervisorAllowedTools("/opt/strawboss", nil)
	have := map[string]bool{}
	for _, a := range got {
		have[a] = true
	}
	for _, want := range []string{
		"Bash(/opt/strawboss delegate:*)",
		"Read", "Edit", "Write", "Glob",
		"Grep",                  // 19% of its calls were denied
		"WebFetch", "WebSearch", // 100% denied
		"Bash(ls:*)", "Bash(wc:*)", "Bash(find:*)",
		"Bash(git status:*)", "Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)",
	} {
		if !have[want] {
			t.Errorf("allowlist missing %q", want)
		}
	}
	// Nothing that writes, executes arbitrary code, or reads file content
	// past Read's path deny rules got in with them.
	for _, unwanted := range []string{"Bash", "Bash(:*)", "Bash(python3:*)", "Bash(rm:*)",
		"Bash(git push:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)"} {
		if have[unwanted] {
			t.Errorf("allowlist unexpectedly contains %q", unwanted)
		}
	}
	// Config extras still append, never replace the baseline.
	withExtra := supervisorAllowedTools("/opt/strawboss", []string{"Bash(make:*)"})
	if len(withExtra) != len(got)+1 || withExtra[len(withExtra)-1] != "Bash(make:*)" {
		t.Errorf("extras did not append cleanly: %v", withExtra)
	}
}

// TestSupervisorDisallowedTools: worker transcripts live under the state
// dir, and invariant 3 keeps them out of supervisor context. Read is
// broadly allowed, so only a deny rule closes it — and the rule has to use
// the //abs form, because a plain absolute path is read as relative to cwd
// and silently matches nothing (verified against the real CLI).
func TestSupervisorDisallowedTools(t *testing.T) {
	tests := []struct {
		name, stateDir, want string
	}{
		{"typical state dir", "/home/u/.strawboss", "Read(//home/u/.strawboss/**)"},
		{"trailing slash", "/home/u/.strawboss/", "Read(//home/u/.strawboss/**)"},
		{"nested temp dir", "/tmp/x/y", "Read(//tmp/x/y/**)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := supervisorDisallowedTools(tt.stateDir)
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("supervisorDisallowedTools(%q) = %v, want [%s]", tt.stateDir, got, tt.want)
			}
			// The form that silently fails must never be produced.
			if strings.HasPrefix(got[0], "Read(/") && !strings.HasPrefix(got[0], "Read(//") {
				t.Errorf("%q uses the plain-absolute form, which matches nothing", got[0])
			}
		})
	}
}

// TestAllowlistGrantsNoPathBlindFileReader: cat/head would read a worker
// transcript straight past the Read deny rule, since Bash rules cannot be
// path-scoped.
func TestAllowlistGrantsNoPathBlindFileReader(t *testing.T) {
	for _, a := range supervisorAllowedTools("/opt/strawboss", nil) {
		for _, bad := range []string{"Bash(cat:", "Bash(head:", "Bash(tail:", "Bash(less:", "Bash(more:"} {
			if strings.HasPrefix(a, bad) {
				t.Errorf("allowlist contains %q — it reads file content outside Read's deny rules", a)
			}
		}
	}
}
