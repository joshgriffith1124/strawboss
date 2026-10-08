package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/router"
)

// modelScore is one row of the stats scorecard.
type modelScore struct {
	Model    string
	Attempts int // finished workers on this model
	Done     int
	Failed   int
	Rescued  int            // failed here, and the SAME task in the SAME run later finished done on a DIFFERENT model
	Median   time.Duration  // median DurationMS of this model's DONE workers; 0 when none
	Bands    map[int][2]int // band -> {done, attempts}
}

// scorecard pairs each worker's spawned event with its finished event by
// worker id and rolls up the completed workers per model. Workers with no
// finished event are ignored. The run filter (empty = all runs) and since
// filter (zero time = no limit) apply to the spawned event's Run/TS.
func scorecard(events []registry.Event, run string, since time.Time) []modelScore {
	type spawnRec struct {
		model, task, run string
		ts               time.Time
	}
	type doneRec struct {
		status string
		dur    time.Duration
	}
	spawns := map[string]spawnRec{}
	dones := map[string]doneRec{}
	for _, ev := range events {
		switch ev.Type {
		case "spawned":
			spawns[ev.Worker] = spawnRec{ev.Model, ev.Task, ev.Run, ev.TS}
		case "finished":
			dones[ev.Worker] = doneRec{ev.Status, time.Duration(ev.DurationMS) * time.Millisecond}
		}
	}

	// Completed workers that pass the filters, in arbitrary order.
	type rec struct {
		spawnRec
		status string
		dur    time.Duration
	}
	var workers []rec
	for id, s := range spawns {
		if run != "" && s.run != run {
			continue
		}
		if !since.IsZero() && s.ts.Before(since) {
			continue
		}
		d, ok := dones[id]
		if !ok {
			continue
		}
		workers = append(workers, rec{s, d.status, d.dur})
	}

	byModel := map[string]*modelScore{}
	doneDurs := map[string][]time.Duration{}
	for _, w := range workers {
		m := byModel[w.model]
		if m == nil {
			m = &modelScore{Model: w.model, Bands: map[int][2]int{}}
			byModel[w.model] = m
		}
		m.Attempts++
		score, _ := router.Score(w.task)
		band := score / 2
		if band > 3 {
			band = 3 // bands stay 0..3 no matter how heavy the task
		}
		b := m.Bands[band]
		b[1]++
		if w.status == "done" {
			b[0]++
		}
		m.Bands[band] = b
		switch w.status {
		case "done":
			m.Done++
			doneDurs[w.model] = append(doneDurs[w.model], w.dur)
		case "failed":
			m.Failed++
		}
	}

	// Rescues: a later-spawned worker on the same task in the same run that
	// finished done on a different model.
	for _, w := range workers {
		if w.status != "failed" {
			continue
		}
		for _, w2 := range workers {
			if w2.model == w.model || w2.task != w.task || w2.run != w.run {
				continue
			}
			if w2.status == "done" && w2.ts.After(w.ts) {
				byModel[w.model].Rescued++
				break
			}
		}
	}

	out := make([]modelScore, 0, len(byModel))
	for model, m := range byModel {
		d := doneDurs[model]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		if n := len(d); n > 0 {
			m.Median = d[(n-1)/2] // even count: lower middle
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// splitByRole keys each role worker's model as "model · role", so the
// scorecard shows which model does well in which role — the evidence for
// whether a role or pipeline is worth its extra runs. Plain tasks keep the
// bare model name.
func splitByRole(events []registry.Event) []registry.Event {
	out := make([]registry.Event, len(events))
	for i, ev := range events {
		if ev.Type == "spawned" && ev.Role != "" {
			ev.Model += " · " + ev.Role
		}
		out[i] = ev
	}
	return out
}

// runStats prints the per-model success rate and routing scorecard from the
// JSONL worker history: outcomes per model, which failures a later attempt on
// another model completed (rescued), and success by auto-score task-weight
// band.
func runStats(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
	run := fs.String("run", "", "only events from this run (default: all runs)")
	since := fs.Duration("since", 0, "only workers spawned within this long before now, e.g. 72h (default: no limit)")
	byRole := fs.Bool("by-role", false, "split each model's row by roles.toml role (\"model · role\")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		var err error
		if *stateDir, err = config.DefaultStateDir(); err != nil {
			return err
		}
	}
	reg := &registry.Registry{Path: filepath.Join(*stateDir, "workers.jsonl")}
	events, err := reg.Load()
	if err != nil {
		return err
	}
	var sinceTime time.Time
	if *since > 0 {
		sinceTime = time.Now().Add(-*since)
	}

	if *byRole {
		events = splitByRole(events)
	}
	scores := scorecard(events, *run, sinceTime)
	if len(scores) == 0 {
		fmt.Fprintln(stdout, "no finished workers in range")
		return nil
	}

	// First table: per-model outcomes.
	w := 17
	for _, m := range scores {
		if len(m.Model)+2 > w {
			w = len(m.Model) + 2
		}
	}
	fmt.Fprintf(stdout, "%-*s %-7s %-6s %-7s %-5s %-9s %s\n",
		w, "MODEL", "TASKS", "DONE", "FAIL", "OK%", "RESCUED", "MEDIAN")
	for _, m := range scores {
		pct := 0
		if m.Attempts > 0 {
			pct = 100 * m.Done / m.Attempts
		}
		fmt.Fprintf(stdout, "%-*s %-7d %-6d %-7d %-5d %-9d %s\n",
			w, m.Model, m.Attempts, m.Done, m.Failed, pct, m.Rescued, m.Median.Round(time.Second))
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "success by task weight (auto-score band):")
	bw := 6
	fmt.Fprintf(stdout, "%-*s", bw, "BAND")
	for _, m := range scores {
		fmt.Fprintf(stdout, " %-*s", len(m.Model)+2, m.Model)
	}
	fmt.Fprintln(stdout)
	for band, label := range []string{"0-1", "2-3", "4-5", "6+"} {
		anyAttempts := false
		for _, m := range scores {
			if m.Bands[band][1] > 0 {
				anyAttempts = true
				break
			}
		}
		if !anyAttempts {
			continue
		}
		fmt.Fprintf(stdout, "%-*s", bw, label)
		for _, m := range scores {
			cell := "-"
			if d, a := m.Bands[band][0], m.Bands[band][1]; a > 0 {
				cell = fmt.Sprintf("%d/%d", d, a)
			}
			fmt.Fprintf(stdout, " %-*s", len(m.Model)+2, cell)
		}
		fmt.Fprintln(stdout)
	}

	fmt.Fprintln(stdout, "RESCUED = failures a later attempt on another model completed; a high count on a cheap model means escalation is doing its job, a low OK% in a band means auto-routing sends that weight too low.")
	return nil
}
