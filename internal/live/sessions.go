package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Per-project session history: one JSONL line per supervisor session,
// appended at init, so the picker can list and switch between past
// conversations of THIS project (never another project's — the picker
// must not reintroduce the cross-project resume bug).

// sessionRecord is one line of projects/<hash>/sessions.jsonl.
type sessionRecord struct {
	TS      time.Time `json:"ts"`
	Session string    `json:"session"`
	Run     string    `json:"run"`
	Label   string    `json:"label,omitempty"` // first prompt, truncated
}

func (o *Orchestrator) sessionsLogPath() string {
	return filepath.Join(ProjectDir(o.StateDir, o.Driver.Dir), "sessions.jsonl")
}

// appendSessionHistory records a session once (respawns of the same
// session re-emit init; only a NEW session id appends).
func (o *Orchestrator) appendSessionHistory(session, run, label string) {
	if session == "" {
		return
	}
	records := o.readSessionHistory()
	for _, r := range records {
		if r.Session == session {
			return
		}
	}
	path := o.sessionsLogPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(label) > 80 {
		label = label[:80] + "…"
	}
	_ = json.NewEncoder(f).Encode(sessionRecord{TS: time.Now(), Session: session, Run: run, Label: label})
}

func (o *Orchestrator) readSessionHistory() []sessionRecord {
	f, err := os.Open(o.sessionsLogPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sessionRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r sessionRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Session != "" {
			out = append(out, r)
		}
	}
	return out
}

// ListSessions returns this project's sessions newest-first, with each
// run's worker tally joined from the registry (ui hook).
func (o *Orchestrator) ListSessions() []ui.SessionInfo {
	records := o.readSessionHistory()
	type tally struct{ workers, done, failed int }
	tallies := map[string]*tally{}
	reg := &registry.Registry{Path: filepath.Join(o.StateDir, "workers.jsonl")}
	if events, err := reg.Load(); err == nil {
		for _, ev := range events {
			t := tallies[ev.Run]
			if t == nil {
				t = &tally{}
				tallies[ev.Run] = t
			}
			switch ev.Type {
			case "spawned":
				t.workers++
			case "finished":
				if ev.Status == "failed" {
					t.failed++
				} else if ev.Status == "done" {
					t.done++
				}
			}
		}
	}
	o.mu.Lock()
	currentRun := o.RunID
	o.mu.Unlock()
	current := o.Driver.SessionID()
	if current == "" {
		current = LoadSession(o.StateDir, o.Driver.Dir)
	}

	out := make([]ui.SessionInfo, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		info := ui.SessionInfo{
			ID: r.Session, Run: r.Run, Started: r.TS, Label: r.Label,
			Current: r.Session == current || (r.Run != "" && r.Run == currentRun),
		}
		if t := tallies[r.Run]; t != nil {
			info.Workers, info.Done, info.Failed = t.workers, t.done, t.failed
		}
		out = append(out, info)
	}
	return out
}

// SwitchSession moves this project to another of its recorded sessions:
// the current supervisor stream ends gracefully (resumable), the session
// and run pointers repoint (persisted, so a restart stays here), worker
// state resets, and the registry replays the chosen run's history. The
// conversation itself resumes on the next prompt.
func (o *Orchestrator) SwitchSession(session, run string) {
	o.endStreamAndResetWorkers(run)
	o.Driver.SetSessionID(session)

	slot := ProjectDir(o.StateDir, o.Driver.Dir)
	_ = os.MkdirAll(slot, 0o755)
	_ = os.WriteFile(filepath.Join(slot, "supervisor-session"), []byte(session), 0o644)
	if run != "" {
		_ = os.WriteFile(filepath.Join(slot, "run"), []byte(run), 0o644)
	}

	o.emitAsync(ui.SessionSwitchedMsg{ID: session})
	o.seedSupUsage()
	select {
	case o.rewind <- struct{}{}:
	default:
	}
	o.emitAsync(ui.RawLogMsg{Source: "app", Line: fmt.Sprintf("switched to session %s (run %s)", session, run)})
}

// NewSession abandons the current conversation for a brand-new one: the
// in-TUI equivalent of restarting with --new, for shedding a bloated
// context. The old session stays in history (the picker can return to
// it); the session pointer clears, a fresh run id is minted (empty
// worker table, zero ledger), and any budget stop from the old run
// lifts. The new conversation starts on the next prompt.
func (o *Orchestrator) NewSession() {
	run, err := RunID(o.StateDir, o.Driver.Dir, true)
	if err != nil {
		o.emitAsync(ui.RawLogMsg{Source: "app", Line: "fresh session: " + err.Error()})
		return
	}
	o.endStreamAndResetWorkers(run)
	o.mu.Lock()
	o.lastPrompt = "" // the next prompt labels the new session
	o.mu.Unlock()
	o.Driver.SetSessionID("")
	_ = os.Remove(filepath.Join(ProjectDir(o.StateDir, o.Driver.Dir), "supervisor-session"))
	_ = os.Remove(BudgetStopFile(o.StateDir, o.Driver.Dir))

	o.emitAsync(ui.SessionSwitchedMsg{})
	o.seedSupUsage()
	select {
	case o.rewind <- struct{}{}:
	default:
	}
	o.emitAsync(ui.RawLogMsg{Source: "app", Line: "fresh session (run " + run + ")"})
}

// endStreamAndResetWorkers gracefully ends the current supervisor stream
// (session stays resumable), repoints the run, and drops all in-memory
// worker state — the shared half of switching and starting sessions.
func (o *Orchestrator) endStreamAndResetWorkers(run string) {
	o.mu.Lock()
	stream := o.stream
	o.stream = nil
	o.RunID = run
	o.sessionToWorker = map[string]string{}
	o.workerSession = map[string]string{}
	o.workerModel = map[string]string{}
	o.workerDir = map[string]string{}
	o.workerTask = map[string]string{}
	o.workerPID = map[string]int{}
	o.unfinished = map[string]bool{}
	o.tailing = map[string]bool{}
	o.dshOut = map[string]int{}
	o.mu.Unlock()

	if stream != nil {
		stream.Shutdown(3 * time.Second)
	}
	// The delegate stamps registry events with STRAWBOSS_RUN inherited
	// through the supervisor's env; left stale, every new worker lands in
	// the OLD run and the watcher (scoped to the new one) silently drops
	// it — workers run while the table says "no workers yet". The stream
	// is down here, so the next spawn picks the new run up.
	o.Driver.SetEnvVar("STRAWBOSS_RUN", run)
}
