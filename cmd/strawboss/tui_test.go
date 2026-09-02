package main

import (
	"reflect"
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
		"Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(wc:*)", "Bash(find:*)",
		"Bash(git status:*)", "Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)",
	} {
		if !have[want] {
			t.Errorf("allowlist missing %q", want)
		}
	}
	// Nothing that writes or executes arbitrary code got in with them.
	for _, unwanted := range []string{"Bash", "Bash(:*)", "Bash(python3:*)", "Bash(rm:*)", "Bash(git push:*)"} {
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
