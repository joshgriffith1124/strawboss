package main

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/registry"
)

func evSpawn(worker, run, model, task string, at time.Time) registry.Event {
	return registry.Event{TS: at, Type: "spawned", Worker: worker, Run: run, Model: model, Task: task}
}

func evFinish(worker, run, status string, at time.Time, durMS int64) registry.Event {
	return registry.Event{TS: at, Type: "finished", Worker: worker, Run: run, Status: status, DurationMS: durMS}
}

// TestSplitByRole: --by-role keys role workers as "model · role" and
// leaves plain tasks on the bare model.
func TestSplitByRole(t *testing.T) {
	now := time.Now()
	reviewer := evSpawn("w1", "r", "big", "t1", now)
	reviewer.Role = "reviewer"
	events := []registry.Event{
		reviewer, evFinish("w1", "r", "done", now, 1000),
		evSpawn("w2", "r", "big", "t2", now), evFinish("w2", "r", "failed", now, 1000),
	}
	var models []string
	for _, s := range scorecard(splitByRole(events), "", time.Time{}) {
		models = append(models, s.Model)
	}
	if want := []string{"big", "big · reviewer"}; !reflect.DeepEqual(models, want) {
		t.Errorf("rows = %v, want %v", models, want)
	}
	if events[0].Model != "big" {
		t.Error("splitByRole mutated its input")
	}
}

func TestScorecard(t *testing.T) {
	base := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	heavy := "refactor the parser module" // score 2 -> band 1
	light := "rename foo to bar"          // score 0 -> band 0
	heavyCapped := strings.Repeat("word ", 350) +
		"refactor foo.go bar.go baz.go qux.go performance security" // score 8 -> band 4 -> capped at 3

	cases := []struct {
		name   string
		events []registry.Event
		run    string
		since  time.Time
		want   map[string]modelScore
	}{
		{
			name: "rescue across models",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evFinish("w1", "r1", "failed", at(1), 3000),
				evSpawn("w2", "r1", "big", heavy, at(2)),
				evFinish("w2", "r1", "done", at(3), 9000),
			},
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Failed: 1, Rescued: 1,
					Bands: map[int][2]int{1: {0, 1}}},
				"big": {Model: "big", Attempts: 1, Done: 1, Median: 9 * time.Second,
					Bands: map[int][2]int{1: {1, 1}}},
			},
		},
		{
			name: "different run does not rescue",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evFinish("w1", "r1", "failed", at(1), 3000),
				evSpawn("w2", "r2", "big", heavy, at(2)),
				evFinish("w2", "r2", "done", at(3), 9000),
			},
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Failed: 1,
					Bands: map[int][2]int{1: {0, 1}}},
				"big": {Model: "big", Attempts: 1, Done: 1, Median: 9 * time.Second,
					Bands: map[int][2]int{1: {1, 1}}},
			},
		},
		{
			name: "same model does not rescue",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evFinish("w1", "r1", "failed", at(1), 3000),
				evSpawn("w2", "r1", "fast", heavy, at(2)),
				evFinish("w2", "r1", "done", at(3), 4000),
			},
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 2, Done: 1, Failed: 1, Median: 4 * time.Second,
					Bands: map[int][2]int{1: {1, 2}}},
			},
		},
		{
			name: "unfinished worker ignored",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evSpawn("w2", "r1", "fast", heavy, at(1)),
				evFinish("w2", "r1", "done", at(2), 1000),
			},
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Done: 1, Median: time.Second,
					Bands: map[int][2]int{1: {1, 1}}},
			},
		},
		{
			name: "median of three done workers is the middle",
			events: []registry.Event{
				evSpawn("w1", "r1", "big", heavy, at(0)),
				evFinish("w1", "r1", "done", at(3), 1000),
				evSpawn("w2", "r1", "big", heavy, at(1)),
				evFinish("w2", "r1", "done", at(4), 5000),
				evSpawn("w3", "r1", "big", heavy, at(2)),
				evFinish("w3", "r1", "done", at(5), 9000),
			},
			want: map[string]modelScore{
				"big": {Model: "big", Attempts: 3, Done: 3, Median: 5 * time.Second,
					Bands: map[int][2]int{1: {3, 3}}},
			},
		},
		{
			name: "run filter",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evFinish("w1", "r1", "done", at(1), 1000),
				evSpawn("w2", "r2", "big", heavy, at(0)),
				evFinish("w2", "r2", "done", at(1), 2000),
			},
			run: "r1",
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Done: 1, Median: time.Second,
					Bands: map[int][2]int{1: {1, 1}}},
			},
		},
		{
			name: "since filter",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", heavy, at(0)),
				evFinish("w1", "r1", "done", at(1), 1000),
				evSpawn("w2", "r1", "fast", heavy, at(30)),
				evFinish("w2", "r1", "done", at(31), 2000),
			},
			since: at(15),
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Done: 1, Median: 2 * time.Second,
					Bands: map[int][2]int{1: {1, 1}}},
			},
		},
		{
			name: "light task lands in band 0",
			events: []registry.Event{
				evSpawn("w1", "r1", "fast", light, at(0)),
				evFinish("w1", "r1", "done", at(1), 1000),
				evSpawn("w2", "r1", "big", heavy, at(0)),
				evFinish("w2", "r1", "failed", at(1), 500),
			},
			want: map[string]modelScore{
				"fast": {Model: "fast", Attempts: 1, Done: 1, Median: time.Second,
					Bands: map[int][2]int{0: {1, 1}}},
				"big": {Model: "big", Attempts: 1, Failed: 1,
					Bands: map[int][2]int{1: {0, 1}}},
			},
		},
		{
			name: "band capped at 3",
			events: []registry.Event{
				evSpawn("w1", "r1", "big", heavyCapped, at(0)),
				evFinish("w1", "r1", "done", at(1), 3000),
			},
			want: map[string]modelScore{
				"big": {Model: "big", Attempts: 1, Done: 1, Median: 3 * time.Second,
					Bands: map[int][2]int{3: {1, 1}}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scorecard(tc.events, tc.run, tc.since)
			if len(got) != len(tc.want) {
				t.Fatalf("scorecard() = %d models, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, m := range got {
				if i > 0 && got[i-1].Model >= m.Model {
					t.Fatalf("models not in ascending name order at %d: %+v", i, got)
				}
				want, ok := tc.want[m.Model]
				if !ok {
					t.Fatalf("unexpected model %q: %+v", m.Model, m)
				}
				if !reflect.DeepEqual(m, want) {
					t.Errorf("model %q:\n got  %+v\n want %+v", m.Model, m, want)
				}
			}
		})
	}
}

func TestRunStats(t *testing.T) {
	t.Run("from state dir", func(t *testing.T) {
		dir := t.TempDir()
		reg := &registry.Registry{Path: filepath.Join(dir, "workers.jsonl"), Run: "r1"}
		id, err := reg.Allocate("s1", "fast", "rename foo to bar", "", 0)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if err := reg.Finish(id, "s1", "done", "", "", 2*time.Second, 10, 0, 5); err != nil {
			t.Fatalf("Finish: %v", err)
		}

		var buf bytes.Buffer
		if err := runStats([]string{"--state-dir", dir}, &buf); err != nil {
			t.Fatalf("runStats: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"MODEL", "fast", "success by task weight"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("empty history", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runStats([]string{"--state-dir", t.TempDir()}, &buf); err != nil {
			t.Fatalf("runStats: %v", err)
		}
		if got := buf.String(); got != "no finished workers in range\n" {
			t.Errorf("output = %q, want %q", got, "no finished workers in range\n")
		}
	})
}
