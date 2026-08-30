package main

import (
	"reflect"
	"testing"
)

func TestSupervisorAllowedTools(t *testing.T) {
	base := []string{"Bash(/opt/strawboss delegate:*)", "Read", "Edit", "Write", "Glob"}
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
