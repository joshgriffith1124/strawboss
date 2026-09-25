package main

import (
	"reflect"
	"testing"
)

func TestStrayFiles(t *testing.T) {
	tests := []struct {
		name  string
		added []string
		tasks []string
		want  []string
	}{
		{
			name:  "path mentioned verbatim",
			added: []string{"pkg/foo.go", "zz_leftover.go"},
			tasks: []string{"update pkg/foo.go to handle x"},
			want:  []string{"zz_leftover.go"},
		},
		{
			name:  "base name mentioned",
			added: []string{"deep/dir/foo.go"},
			tasks: []string{"edit foo.go"},
			want:  nil,
		},
		{
			name:  "parent dir mentioned",
			added: []string{"pkg/foo.go"},
			tasks: []string{"add a helper to the pkg directory"},
			want:  nil,
		},
		{
			name:  "go test file covered by the file under test",
			added: []string{"pkg/foo_test.go"},
			tasks: []string{"edit foo.go"},
			want:  nil,
		},
		{
			name:  "mentioned only in another task of the batch",
			added: []string{"shared/util.go"},
			tasks: []string{"fix the parser bug", "write shared/util.go caching"},
			want:  nil,
		},
		{
			name:  "unmentioned file flagged",
			added: []string{"zz_repro_test.go"},
			tasks: []string{"fix the parser bug"},
			want:  []string{"zz_repro_test.go"},
		},
		{
			name:  "root-level file not auto-covered by its '.' dir",
			added: []string{"notes.md"},
			tasks: []string{"do the thing."},
			want:  []string{"notes.md"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strayFiles(tc.added, tc.tasks); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("strayFiles(%q, %q) = %v, want %v", tc.added, tc.tasks, got, tc.want)
			}
		})
	}
}

func TestStrayNote(t *testing.T) {
	tests := []struct {
		name  string
		stray []string
		want  string
	}{
		{"none", nil, ""},
		{"two",
			[]string{"a.go", "b/c_test.go"},
			"[new files not named in the task: a.go, b/c_test.go — delete them if they are leftovers]"},
		{"seven",
			[]string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go"},
			"[new files not named in the task: a.go, b.go, c.go, d.go, e.go, and 2 more — delete them if they are leftovers]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strayNote(tc.stray); got != tc.want {
				t.Fatalf("strayNote = %q, want %q", got, tc.want)
			}
		})
	}
}
