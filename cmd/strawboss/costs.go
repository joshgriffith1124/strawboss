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
)

// runCosts recomputes the token economy from the replayable JSONL
// history: one line per run (workers, outcomes, tokens, wall time). The
// supervisor's plan-metered side lives in its own stream and isn't
// persisted here — this is the free local side of the ledger.
func runCosts(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("costs", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory (default ~/.strawboss)")
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
	if len(events) == 0 {
		fmt.Fprintln(stdout, "no worker history yet")
		return nil
	}

	type tally struct {
		first, last         time.Time
		workers, done, fail int
		in, out             int
		wall                time.Duration
	}
	runs := map[string]*tally{}
	var order []string
	for _, ev := range events {
		run := ev.Run
		if run == "" {
			run = "(unscoped)"
		}
		t := runs[run]
		if t == nil {
			t = &tally{first: ev.TS}
			runs[run] = t
			order = append(order, run)
		}
		t.last = ev.TS
		switch ev.Type {
		case "spawned":
			t.workers++
		case "finished":
			switch ev.Status {
			case "done":
				t.done++
			case "failed":
				t.fail++
			}
			t.in += ev.InputTokens
			t.out += ev.OutputTokens
			t.wall += time.Duration(ev.DurationMS) * time.Millisecond
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return runs[order[i]].first.Before(runs[order[j]].first) })

	fmt.Fprintf(stdout, "%-22s %-16s %8s %5s %5s %10s %10s %9s\n",
		"RUN", "STARTED", "WORKERS", "DONE", "FAIL", "TOK-IN", "TOK-OUT", "WORK-TIME")
	for _, run := range order {
		t := runs[run]
		fmt.Fprintf(stdout, "%-22s %-16s %8d %5d %5d %10d %10d %9s\n",
			run, t.first.Local().Format("2006-01-02 15:04"),
			t.workers, t.done, t.fail, t.in, t.out, t.wall.Round(time.Second))
	}
	fmt.Fprintln(stdout, "\nworker tokens are local and free; the supervisor's plan-metered side shows live in the TUI")
	return nil
}
