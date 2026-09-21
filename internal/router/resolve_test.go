package router

import (
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

// down builds an availability check from the set of model names that are
// down; everything else comes up.
func down(names ...string) func(config.ModelConfig) bool {
	bad := make(map[string]bool, len(names))
	for _, n := range names {
		bad[n] = true
	}
	return func(m config.ModelConfig) bool { return !bad[m.Name] }
}

// model builds a named config for the test lists.
func model(name string, tier int) config.ModelConfig {
	return config.ModelConfig{Name: name, Tier: tier}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		models   []config.ModelConfig
		want     config.ModelConfig
		avail    func(config.ModelConfig) bool
		wantName string
		ok       bool
	}{
		{
			name:     "tiered, want available",
			models:   []config.ModelConfig{model("fast", 1), model("spark-a", 2), model("spark-b", 2)},
			want:     model("fast", 1),
			avail:    down(),
			wantName: "fast",
			ok:       true,
		},
		{
			name:     "untiered, want available",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			want:     model("b", 0),
			avail:    down(),
			wantName: "b",
			ok:       true,
		},
		{
			name:     "tiered, want down picks up a tier",
			models:   []config.ModelConfig{model("fast", 1), model("spark-a", 2), model("spark-b", 2)},
			want:     model("fast", 1),
			avail:    down("fast"),
			wantName: "spark-a",
			ok:       true,
		},
		{
			name:     "same-tier sibling beats anything else",
			models:   []config.ModelConfig{model("fast", 1), model("spark-a", 2), model("spark-b", 2)},
			want:     model("spark-a", 2),
			avail:    down("spark-a"),
			wantName: "spark-b",
			ok:       true,
		},
		{
			name:     "falls down as a last resort",
			models:   []config.ModelConfig{model("fast", 1), model("spark-a", 2), model("spark-b", 2)},
			want:     model("spark-a", 2),
			avail:    down("spark-a", "spark-b"),
			wantName: "fast",
			ok:       true,
		},
		{
			name:     "nearest higher tier, not slice order",
			models:   []config.ModelConfig{model("fast", 1), model("huge", 3), model("big", 2)},
			want:     model("fast", 1),
			avail:    down("fast"),
			wantName: "big",
			ok:       true,
		},
		{
			name:     "everything down",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2)},
			want:     model("fast", 1),
			avail:    down("fast", "big"),
			wantName: "fast",
			ok:       false,
		},
		{
			name:     "untiered, next entry",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			want:     model("b", 0),
			avail:    down("b"),
			wantName: "c",
			ok:       true,
		},
		{
			name:     "untiered, falls back to earlier entries",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			want:     model("b", 0),
			avail:    down("b", "c"),
			wantName: "a",
			ok:       true,
		},
		{
			name:     "nil avail returns want",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2)},
			want:     model("big", 2),
			wantName: "big",
			ok:       true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Resolve(tc.models, tc.want, tc.avail)
			if got.Name != tc.wantName {
				t.Errorf("Resolve name = %q, want %q", got.Name, tc.wantName)
			}
			if ok != tc.ok {
				t.Errorf("Resolve ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		name     string
		models   []config.ModelConfig
		cur      string
		avail    func(config.ModelConfig) bool
		wantName string
		ok       bool
	}{
		{
			name:     "untiered, next entry",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			cur:      "a",
			avail:    down(),
			wantName: "b",
			ok:       true,
		},
		{
			name:     "untiered, skips the down one",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			cur:      "a",
			avail:    down("b"),
			wantName: "c",
			ok:       true,
		},
		{
			name:     "untiered, last entry has nowhere to go",
			models:   []config.ModelConfig{model("a", 0), model("b", 0), model("c", 0)},
			cur:      "c",
			avail:    down(),
			wantName: "",
			ok:       false,
		},
		{
			name:     "unknown cur",
			models:   []config.ModelConfig{model("a", 0), model("b", 0)},
			cur:      "zzz",
			avail:    down(),
			wantName: "",
			ok:       false,
		},
		{
			name:     "tiered, one rung up",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2)},
			cur:      "fast",
			avail:    down(),
			wantName: "big",
			ok:       true,
		},
		{
			name:     "tiered, top rung has nowhere to go",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2)},
			cur:      "big",
			avail:    down(),
			wantName: "",
			ok:       false,
		},
		{
			name:     "same-tier sibling skipped",
			models:   []config.ModelConfig{model("spark-a", 2), model("spark-b", 2), model("fast", 1), model("huge", 3)},
			cur:      "spark-a",
			avail:    down(),
			wantName: "huge",
			ok:       true,
		},
		{
			name:     "skips the dead tier",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2), model("huge", 3)},
			cur:      "fast",
			avail:    down("big"),
			wantName: "huge",
			ok:       true,
		},
		{
			name:     "slice order within a tier",
			models:   []config.ModelConfig{model("fast", 1), model("big-b", 2), model("big-a", 2)},
			cur:      "fast",
			avail:    down(),
			wantName: "big-b",
			ok:       true,
		},
		{
			name:     "slice order within a tier skips the down one",
			models:   []config.ModelConfig{model("fast", 1), model("big-b", 2), model("big-a", 2)},
			cur:      "fast",
			avail:    down("big-b"),
			wantName: "big-a",
			ok:       true,
		},
		{
			name:     "higher tier declared earlier in the slice",
			models:   []config.ModelConfig{model("big", 2), model("fast", 1)},
			cur:      "fast",
			avail:    down(),
			wantName: "big",
			ok:       true,
		},
		{
			name:     "tiered, only rung up is down",
			models:   []config.ModelConfig{model("fast", 1), model("big", 2)},
			cur:      "fast",
			avail:    down("big"),
			wantName: "",
			ok:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Next(tc.models, tc.cur, tc.avail)
			if got.Name != tc.wantName {
				t.Errorf("Next name = %q, want %q", got.Name, tc.wantName)
			}
			if ok != tc.ok {
				t.Errorf("Next ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}
