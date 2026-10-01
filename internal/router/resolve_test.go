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

// freeNames builds a has-a-free-slot check from the set of model names that
// have one; everything else counts as saturated.
func freeNames(names ...string) func(config.ModelConfig) bool {
	free := make(map[string]bool, len(names))
	for _, n := range names {
		free[n] = true
	}
	return func(m config.ModelConfig) bool { return free[m.Name] }
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

func TestPreferFree(t *testing.T) {
	cases := []struct {
		name     string
		models   []config.ModelConfig
		got      config.ModelConfig
		avail    func(config.ModelConfig) bool
		free     func(config.ModelConfig) bool
		wantName string
	}{
		{
			name:     "saturated pick with a free sibling switches",
			models:   []config.ModelConfig{model("spark-a", 2), model("spark-b", 2)},
			got:      model("spark-a", 2),
			avail:    down(),
			free:     freeNames("spark-b"),
			wantName: "spark-b",
		},
		{
			name:     "no free sibling keeps got",
			models:   []config.ModelConfig{model("spark-a", 2), model("spark-b", 2)},
			got:      model("spark-a", 2),
			avail:    down(),
			free:     freeNames(),
			wantName: "spark-a",
		},
		{
			name:     "a free model on a different tier is never chosen",
			models:   []config.ModelConfig{model("fast", 1), model("spark-a", 2)},
			got:      model("spark-a", 2),
			avail:    down(),
			free:     freeNames("fast"),
			wantName: "spark-a",
		},
		{
			name:     "an unavailable sibling is skipped",
			models:   []config.ModelConfig{model("spark-a", 2), model("spark-b", 2), model("spark-c", 2)},
			got:      model("spark-a", 2),
			avail:    down("spark-b"),
			free:     freeNames("spark-b", "spark-c"),
			wantName: "spark-c",
		},
		{
			name:     "nil free returns got",
			models:   []config.ModelConfig{model("spark-a", 2), model("spark-b", 2)},
			got:      model("spark-a", 2),
			avail:    down(),
			free:     nil,
			wantName: "spark-a",
		},
		{
			name:     "untiered list returns got",
			models:   []config.ModelConfig{model("a", 0), model("b", 0)},
			got:      model("a", 0),
			avail:    down(),
			free:     freeNames("b"),
			wantName: "a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreferFree(tc.models, tc.got, tc.avail, tc.free); got.Name != tc.wantName {
				t.Errorf("PreferFree name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

func TestBalance(t *testing.T) {
	pooled := []config.ModelConfig{
		{Name: "a", Tier: 1, Pool: "qwen"},
		{Name: "b", Tier: 1, Pool: "qwen"},
	}

	// loadNames builds a busy count per model name (missing = 0).
	loadNames := func(counts map[string]int) func(config.ModelConfig) int {
		return func(m config.ModelConfig) int { return counts[m.Name] }
	}

	t.Run("no pool returns got unchanged", func(t *testing.T) {
		got := model("solo", 1)
		if bal := Balance(pooled, got, down(), loadNames(map[string]int{"solo": 9}), nil); bal.Name != "solo" {
			t.Errorf("Balance name = %q, want solo", bal.Name)
		}
	})
	t.Run("even loads keep the first member", func(t *testing.T) {
		if bal := Balance(pooled, pooled[1], down(), loadNames(nil), nil); bal.Name != "a" {
			t.Errorf("Balance name = %q, want a (file order wins ties)", bal.Name)
		}
	})
	t.Run("busier member sheds to the idle one", func(t *testing.T) {
		if bal := Balance(pooled, pooled[0], down(), loadNames(map[string]int{"a": 1}), nil); bal.Name != "b" {
			t.Errorf("Balance name = %q, want b", bal.Name)
		}
	})
	t.Run("an unavailable member is skipped", func(t *testing.T) {
		if bal := Balance(pooled, pooled[0], down("b"), loadNames(map[string]int{"a": 1}), nil); bal.Name != "a" {
			t.Errorf("Balance name = %q, want a (b is down)", bal.Name)
		}
	})
	t.Run("a free member beats a lower-load-but-full one", func(t *testing.T) {
		if bal := Balance(pooled, pooled[0], down(), loadNames(map[string]int{"a": 0, "b": 1}), freeNames("b")); bal.Name != "b" {
			t.Errorf("Balance name = %q, want b (free beats load)", bal.Name)
		}
	})
	t.Run("nil free falls back to load alone", func(t *testing.T) {
		if bal := Balance(pooled, pooled[0], down(), loadNames(map[string]int{"a": 1}), nil); bal.Name != "b" {
			t.Errorf("Balance name = %q, want b", bal.Name)
		}
	})
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
