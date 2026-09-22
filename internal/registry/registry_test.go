package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAllocateSequentialAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers.jsonl")
	// Two instances on the same file, as two delegate processes would be.
	a := &Registry{Path: path}
	b := &Registry{Path: path}

	id1, err := a.Allocate("ses_1", "qwen-coder", "task one", "/repo", 0)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := b.Allocate("ses_2", "qwen-coder", "task two", "/repo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != "w1" || id2 != "w2" {
		t.Errorf("ids = %s, %s; want w1, w2", id1, id2)
	}
}

func TestAllocateConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers.jsonl")
	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Registry{Path: path}
			id, err := r.Allocate(fmt.Sprintf("ses_%d", i), "m", "t", "/", 0)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate worker id %s in %v", id, ids)
		}
		seen[id] = true
	}
}

func TestFinishLoadReduce(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl"), Run: "run-77"}
	w1, _ := r.Allocate("ses_1", "qwen-coder", "build the thing", "/repo", 0)
	w2, _ := r.Allocate("ses_2", "qwen-small", "docstrings", "/repo", 0)
	if err := r.Finish(w1, "ses_1", "done", "built; tests pass", "/logs/ses_1.jsonl", 95*time.Second, 12000, 0, 800); err != nil {
		t.Fatal(err)
	}

	events, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events", len(events))
	}
	for _, ev := range events {
		if ev.Run != "run-77" {
			t.Errorf("event %s/%s run = %q, want run-77", ev.Type, ev.Worker, ev.Run)
		}
	}
	workers := Reduce(events)
	if len(workers) != 2 {
		t.Fatalf("got %d workers", len(workers))
	}
	got1, got2 := workers[0], workers[1]
	if got1.ID != w1 || got1.Status != "done" || got1.Summary != "built; tests pass" {
		t.Errorf("w1 = %+v", got1)
	}
	if got1.InputTokens != 12000 || got1.OutputTokens != 800 {
		t.Errorf("w1 tokens = %d/%d", got1.InputTokens, got1.OutputTokens)
	}
	if got1.LogPath != "/logs/ses_1.jsonl" {
		t.Errorf("w1 log = %q", got1.LogPath)
	}
	if got2.ID != w2 || got2.Status != "running" {
		t.Errorf("w2 = %+v", got2)
	}
	if got2.Task != "docstrings" || got2.Model != "qwen-small" {
		t.Errorf("w2 = %+v", got2)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "nope.jsonl")}
	events, err := r.Load()
	if err != nil || events != nil {
		t.Fatalf("missing file: events=%v err=%v", events, err)
	}

	path := filepath.Join(t.TempDir(), "workers.jsonl")
	content := `{"ts":"2026-08-28T12:00:00Z","type":"spawned","worker":"w1","session":"s1"}
this line is garbage
{"ts":"2026-08-28T12:01:00Z","type":"finished","worker":"w1","session":"s1","status":"done"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r = &Registry{Path: path}
	events, err = r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (garbage skipped)", len(events))
	}
	workers := Reduce(events)
	if len(workers) != 1 || workers[0].Status != "done" {
		t.Errorf("workers = %+v", workers)
	}
}

func TestReduceFinishedWithoutSpawn(t *testing.T) {
	// A finished event for an unknown worker (truncated log) is ignored.
	workers := Reduce([]Event{{Type: "finished", Worker: "w9", Status: "done"}})
	if len(workers) != 0 {
		t.Errorf("workers = %+v", workers)
	}
}

func TestQueueAlone(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl")}
	id, err := r.Queue("qwen-small", "docstrings", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if id != "w1" {
		t.Errorf("Queue id = %s, want w1", id)
	}
	events, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	workers := Reduce(events)
	if len(workers) != 1 {
		t.Fatalf("got %d workers, want 1: %+v", len(workers), workers)
	}
	w := workers[0]
	if w.ID != "w1" || w.Status != "queued" {
		t.Errorf("worker = %+v, want queued w1", w)
	}
	if w.Model != "qwen-small" || w.Task != "docstrings" || w.Dir != "/repo" {
		t.Errorf("worker = %+v", w)
	}
	if w.Session != "" || !w.Started.IsZero() {
		t.Errorf("worker = %+v, want empty session and zero Started", w)
	}
}

func TestQueueThenAllocateAsThenFinish(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl"), Run: "run-q"}
	// w1's model is at its max_concurrent cap, so it waits queued while w2
	// allocates directly; when a slot opens, w1 spawns under its queued id.
	qid, err := r.Queue("qwen-coder", "build the thing", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if w2, _ := r.Allocate("ses_2", "qwen-small", "docstrings", "/repo", 0); w2 != "w2" {
		t.Fatalf("Allocate after Queue = %s, want w2", w2)
	}
	if err := r.AllocateAs(qid, "ses_1", "qwen-coder", "build the thing", "/repo", 4242); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(qid, "ses_1", "done", "built; tests pass", "/logs/ses_1.jsonl", 95*time.Second, 12000, 0, 800); err != nil {
		t.Fatal(err)
	}

	events, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4 (queued, spawned, spawned, finished)", len(events))
	}
	for _, ev := range events {
		if ev.Run != "run-q" {
			t.Errorf("event %s/%s run = %q, want run-q", ev.Type, ev.Worker, ev.Run)
		}
	}
	var spawnedTS time.Time
	for _, ev := range events {
		if ev.Type == "spawned" && ev.Worker == qid {
			spawnedTS = ev.TS
		}
	}
	workers := Reduce(events)
	if len(workers) != 2 {
		t.Fatalf("got %d workers, want 2: %+v", len(workers), workers)
	}
	w1, w2 := workers[0], workers[1]
	if w1.ID != qid || w1.Status != "done" || w1.Session != "ses_1" {
		t.Errorf("w1 = %+v", w1)
	}
	if !w1.Started.Equal(spawnedTS) {
		t.Errorf("w1 Started = %v, want the spawned event's TS %v", w1.Started, spawnedTS)
	}
	if w1.InputTokens != 12000 || w1.OutputTokens != 800 || w1.LogPath != "/logs/ses_1.jsonl" {
		t.Errorf("w1 = %+v", w1)
	}
	if w2.ID != "w2" || w2.Status != "running" {
		t.Errorf("w2 = %+v", w2)
	}
}

func TestQueueAllocateShareIDSequence(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl")}
	ops := []struct {
		kind string // "allocate" or "queue"
		want string
	}{
		{"allocate", "w1"},
		{"queue", "w2"},
		{"allocate", "w3"},
		{"queue", "w4"},
	}
	for _, op := range ops {
		var got string
		var err error
		switch op.kind {
		case "allocate":
			got, err = r.Allocate("s", "m", "t", "/", 0)
		case "queue":
			got, err = r.Queue("m", "t", "/")
		}
		if err != nil {
			t.Fatalf("%s: %v", op.kind, err)
		}
		if got != op.want {
			t.Errorf("%s returned %s, want %s", op.kind, got, op.want)
		}
	}
}

func TestQueuedFinishedWithoutSpawning(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "workers.jsonl")}
	id, err := r.Queue("qwen-coder", "build the thing", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	// The run is aborted before any slot opens.
	if err := r.Finish(id, "", "failed", "run aborted before a slot opened", "/logs/run.log", time.Minute, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	events, err := r.Load()
	if err != nil {
		t.Fatal(err)
	}
	workers := Reduce(events)
	if len(workers) != 1 {
		t.Fatalf("got %d workers, want 1: %+v", len(workers), workers)
	}
	if workers[0].ID != id || workers[0].Status != "failed" {
		t.Errorf("worker = %+v", workers[0])
	}
	if !workers[0].Started.IsZero() {
		t.Errorf("Started = %v, want zero (never spawned)", workers[0].Started)
	}
}

func TestReduceQueued(t *testing.T) {
	ts := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		events []Event
		want   []Worker
	}{
		{
			name: "queued then spawned updates in place",
			events: []Event{
				{TS: ts, Type: "queued", Worker: "w1", Model: "m-queued", Task: "t-queued", Dir: "/q"},
				{TS: ts.Add(time.Minute), Type: "spawned", Worker: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/"},
			},
			want: []Worker{
				{ID: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/", Status: "running", Started: ts.Add(time.Minute)},
			},
		},
		{
			name: "order follows the first event per worker",
			events: []Event{
				{TS: ts, Type: "queued", Worker: "w1", Model: "m", Task: "t", Dir: "/"},
				{TS: ts, Type: "spawned", Worker: "w2", Session: "s2", Model: "m", Task: "t", Dir: "/"},
				{TS: ts.Add(time.Minute), Type: "spawned", Worker: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/"},
			},
			want: []Worker{
				{ID: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/", Status: "running", Started: ts.Add(time.Minute)},
				{ID: "w2", Session: "s2", Model: "m", Task: "t", Dir: "/", Status: "running", Started: ts},
			},
		},
		{
			name: "unqueued worker unchanged",
			events: []Event{
				{TS: ts, Type: "spawned", Worker: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/"},
				{TS: ts.Add(time.Minute), Type: "finished", Worker: "w1", Status: "done", Summary: "ok"},
			},
			want: []Worker{
				{ID: "w1", Session: "s1", Model: "m", Task: "t", Dir: "/", Status: "done", Summary: "ok", Started: ts, Finished: ts.Add(time.Minute)},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Reduce(tc.events)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d workers, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("worker[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
