package main

import (
	"reflect"
	"testing"
)

func TestFailRefs(t *testing.T) {
	cases := []struct {
		name   string
		output string
		files  []string
		pkgs   []string
	}{
		{
			name: "go build failure",
			output: `# example.com/m/internal/ui
internal/ui/foo.go:12:3: undefined: bar
FAIL	example.com/m/internal/ui [build failed]`,
			files: []string{"internal/ui/foo.go"},
			pkgs:  []string{"example.com/m/internal/ui"},
		},
		{
			name: "go test failure",
			output: `--- FAIL: TestX (0.00s)
    foo_test.go:42: got 1
FAIL
FAIL	example.com/m/internal/live	0.3s`,
			files: []string{"foo_test.go"},
			pkgs:  []string{"example.com/m/internal/live"},
		},
		{
			name:   "dot-slash path stripped",
			output: "./x.go:3: undefined: y",
			files:  []string{"x.go"},
		},
		{
			name: "no refs",
			output: `everything went wrong
FAIL
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, pkgs := failRefs(tc.output)
			if !reflect.DeepEqual(files, tc.files) {
				t.Errorf("files = %v, want %v", files, tc.files)
			}
			if !reflect.DeepEqual(pkgs, tc.pkgs) {
				t.Errorf("pkgs = %v, want %v", pkgs, tc.pkgs)
			}
		})
	}
}

func TestBlame(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		changed [][]string
		tasks   []string
		want    map[int][]string
		wantOK  bool
	}{
		{
			name:    "single culprit by exact path",
			output:  "internal/ui/foo.go:12:3: undefined: bar",
			changed: [][]string{{"internal/ui/foo.go"}, {"internal/live/bar.go"}},
			tasks:   []string{"fix the ui", "fix live"},
			want:    map[int][]string{0: {"internal/ui/foo.go"}},
			wantOK:  true,
		},
		{
			name:    "bare test base name matched by suffix",
			output:  "    foo_test.go:42: got 1\nFAIL\n",
			changed: [][]string{nil, {"internal/live/foo_test.go"}},
			tasks:   []string{"a", "b"},
			want:    map[int][]string{1: {"foo_test.go"}},
			wantOK:  true,
		},
		{
			name:    "shared file resolved by task mention",
			output:  "shared.go:5:2: syntax error",
			changed: [][]string{{"shared.go"}, {"shared.go"}},
			tasks:   []string{"update shared.go logic", "add new feature"},
			want:    map[int][]string{0: {"shared.go"}},
			wantOK:  true,
		},
		{
			name:    "shared file, no task mentions it",
			output:  "shared.go:5:2: syntax error",
			changed: [][]string{{"shared.go"}, {"shared.go"}},
			tasks:   []string{"do a", "do b"},
			want:    map[int][]string{0: {"shared.go"}, 1: {"shared.go"}},
			wantOK:  true,
		},
		{
			name:   "package-level fallback when no file matches",
			output: "# example.com/m/internal/live\nFAIL\texample.com/m/internal/live [build failed]\n",
			changed: [][]string{
				{"internal/ui/foo.go"},
				{"internal/live/foo.go"},
			},
			tasks:  []string{"a", "b"},
			want:   map[int][]string{1: {"internal/live"}},
			wantOK: true,
		},
		{
			name:    "nothing matches",
			output:  "internal/ui/foo.go:12:3: undefined: bar",
			changed: [][]string{{"internal/live/bar.go"}, nil},
			tasks:   []string{"a", "b"},
			want:    nil,
			wantOK:  false,
		},
		{
			name:    "nil changed is safe",
			output:  "internal/ui/foo.go:12:3: undefined: bar",
			changed: [][]string{nil, nil},
			tasks:   []string{"a", "b"},
			want:    nil,
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := blame(tc.output, tc.changed, tc.tasks)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (culprits %v)", ok, tc.wantOK, got)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("culprits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlameNote(t *testing.T) {
	cases := []struct {
		name     string
		culprits map[int][]string
		ids      []string
		want     string
	}{
		{
			name:     "two culprits plus credited workers",
			culprits: map[int][]string{0: {"internal/ui/foo.go"}, 2: {"internal/live"}},
			ids:      []string{"w12", "w11", "w14", "w13"},
			want:     "(points at w12: internal/ui/foo.go; w14: internal/live — not implicated: w11, w13)",
		},
		{
			name:     "all workers culprits",
			culprits: map[int][]string{0: {"a.go"}, 1: {"b.go"}},
			ids:      []string{"w1", "w2"},
			want:     "(points at w1: a.go; w2: b.go)",
		},
		{
			name:     "more than three refs truncated",
			culprits: map[int][]string{0: {"a.go", "b.go", "c.go", "d.go", "e.go"}},
			ids:      []string{"w1", "w2"},
			want:     "(points at w1: a.go, b.go, c.go +2 more — not implicated: w2)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blameNote(tc.culprits, tc.ids)
			if got != tc.want {
				t.Errorf("blameNote = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBlameSortedRefs(t *testing.T) {
	culprits, ok := blame("z.go:1: err\na.go:2: err\nm/b.go:3: err\nn/c.go:4: err\no/d.go:5: err",
		[][]string{{"z.go", "a.go", "m/b.go", "n/c.go", "o/d.go"}},
		[]string{"t"})
	if !ok {
		t.Fatal("expected attribution")
	}
	if !reflect.DeepEqual(culprits[0], []string{"a.go", "m/b.go", "n/c.go", "o/d.go", "z.go"}) {
		t.Errorf("refs not sorted: %v", culprits[0])
	}
}
