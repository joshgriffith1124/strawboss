package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/harness"
	"github.com/joshgriffith1124/strawboss/internal/harness/dshacp"
	"github.com/joshgriffith1124/strawboss/internal/harness/opencode"
	"github.com/joshgriffith1124/strawboss/internal/registry"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// watchRegistry tails workers.jsonl: historical events replay at startup
// (state survives restarts), new events stream as delegations happen.
func (o *Orchestrator) watchRegistry(ctx context.Context) {
	path := filepath.Join(o.StateDir, "workers.jsonl")
	var offset int64
	for {
		offset = o.readRegistryFrom(ctx, path, offset)
		select {
		case <-ctx.Done():
			return
		case <-o.rewind:
			// Session switch: replay the whole file so the new run's
			// history repopulates (events are run-filtered on apply).
			offset = 0
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (o *Orchestrator) readRegistryFrom(ctx context.Context, path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset // not created yet — displayed state is simply "no workers"
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() <= offset {
		return offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	rd := bufio.NewReader(f)
	// A pass from offset 0 (startup or a session-switch rewind) applies
	// the whole history; fold its msgs into ONE ui.BatchMsg so the screen
	// renders once instead of popping tiles per event. Incremental passes
	// keep emitting individually — those events are happening now.
	var batch []tea.Msg
	replay := offset == 0
	defer func() {
		if replay && len(batch) > 0 {
			o.emit(ctx, ui.BatchMsg{Msgs: batch})
		}
	}()
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // partial write in progress — retry from same offset
			return offset
		}
		offset += int64(len(line))
		var ev registry.Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if replay {
			o.applyRegistryEvent(ctx, ev, &batch)
		} else {
			o.applyRegistryEvent(ctx, ev, nil)
		}
	}
}

func (o *Orchestrator) applyRegistryEvent(ctx context.Context, ev registry.Event, batch *[]tea.Msg) {
	o.mu.Lock()
	run := o.RunID
	o.mu.Unlock()
	if run != "" && ev.Run != run {
		return // another run's worker — not this session's story
	}
	switch ev.Type {
	case "queued":
		// Waiting for a concurrency slot: no session yet, so no
		// unfinished entry and no tailer — but record the task so
		// kill/retry can find it, and open the row.
		o.mu.Lock()
		o.workerModel[ev.Worker] = ev.Model
		o.workerDir[ev.Worker] = ev.Dir
		o.workerTask[ev.Worker] = ev.Task
		o.mu.Unlock()
		o.emitBatch(ctx, batch, ui.WorkerUpsertMsg{
			ID: ev.Worker, Model: ev.Model, Task: ev.Task, Dir: ev.Dir,
			Status: "queued",
		})
	case "spawned":
		o.mu.Lock()
		o.sessionToWorker[ev.Session] = ev.Worker
		o.workerSession[ev.Worker] = ev.Session
		o.workerModel[ev.Worker] = ev.Model
		o.workerDir[ev.Worker] = ev.Dir
		o.workerTask[ev.Worker] = ev.Task
		o.workerPID[ev.Worker] = ev.PID
		o.workerStarted[ev.Worker] = ev.TS
		o.unfinished[ev.Worker] = true
		startTail := false
		if mc, ok := o.modelConfig(ev.Model); ok && mc.Harness == "dsh" && !o.tailing[ev.Worker] {
			o.tailing[ev.Worker] = true
			startTail = true
		}
		o.mu.Unlock()
		if startTail {
			go o.tailDshWorker(ctx, ev.Worker, ev.Session)
		}
		o.emitBatch(ctx, batch, ui.WorkerUpsertMsg{
			ID: ev.Worker, Model: ev.Model, Task: ev.Task, Dir: ev.Dir,
			Status: "running", Started: ev.TS,
		})
	case "finished":
		o.mu.Lock()
		delete(o.unfinished, ev.Worker)
		reaped := o.reaped[ev.Worker]
		o.mu.Unlock()
		// One notion of "replayed" for both consumers: history read back
		// at startup must neither push to a phone nor ring the local
		// bell. Restarting after a crash otherwise announces every worker
		// the crash killed as if it were failing right then. A worker
		// reaped as dead by x died in the past too.
		replay := !ev.TS.After(o.started) || reaped
		if ev.Status == "failed" && !replay {
			summary := ev.Summary
			if i := strings.IndexByte(summary, '\n'); i >= 0 {
				summary = summary[:i]
			}
			o.pushFailure(ev.Worker, summary)
		}
		o.emitBatch(ctx, batch,
			ui.WorkerUpsertMsg{ID: ev.Worker, Status: ev.Status, Summary: ev.Summary,
				LogPath: ev.LogPath, Ended: ev.TS, Replay: replay},
			ui.WorkerUsageMsg{ID: ev.Worker, Input: ev.InputTokens, CacheRead: ev.CacheReadTokens, Output: ev.OutputTokens},
		)
	case "deadline":
		o.mu.Lock()
		o.workerDeadline[ev.Worker] = ev.Deadline
		o.mu.Unlock()
		// A bare upsert: only the deadline — the merge must keep the
		// row's other fields.
		o.emitBatch(ctx, batch, ui.WorkerUpsertMsg{ID: ev.Worker, Deadline: ev.Deadline})
	}
}

// emitBatch appends msgs to a replay collector when one is active (a
// first registry pass folds them into one ui.BatchMsg) and emits them
// normally otherwise.
func (o *Orchestrator) emitBatch(ctx context.Context, batch *[]tea.Msg, msgs ...tea.Msg) {
	if batch != nil {
		*batch = append(*batch, msgs...)
		return
	}
	o.emit(ctx, msgs...)
}

// replayFlushDelay is how long a dsh tailer may sit on buffered replayed
// history before flushing it as one ui.BatchMsg anyway.
const replayFlushDelay = 200 * time.Millisecond

// tailDshWorker streams one dsh worker's session log into transcript and
// usage msgs — dsh has no server to poll or subscribe to; the JSONL under
// the persistence root is the observability surface (docs/NOTES.md). The
// tailer ends at turn/end; if the worker is still unfinished then (its
// delegate died — an orphaned dsh subprocess runs to completion on its
// own), the row is closed as recovered like the opencode path.
//
// Replayed history (a session log that already existed when the tail
// began) is buffered and flushed as ONE ui.BatchMsg — on the first live
// item, the tail closing, or 200ms of quiet, whichever comes first — so
// an old transcript doesn't visibly scroll through the tile. Live items
// emit individually.
func (o *Orchestrator) tailDshWorker(ctx context.Context, wid, session string) {
	defer func() {
		o.mu.Lock()
		delete(o.tailing, wid)
		o.mu.Unlock()
	}()
	root := dshacp.SessionsRootFor(o.StateDir)
	endReason := ""
	var pending []tea.Msg
	flush := func() {
		if len(pending) > 0 {
			o.emit(ctx, ui.BatchMsg{Msgs: pending})
			pending = nil
		}
	}
	timer := time.NewTimer(replayFlushDelay)
	defer timer.Stop()
	apply := func(it dshacp.TailItem) {
		switch {
		case it.Event != nil:
			if it.Replay {
				pending = append(pending, ui.WorkerEventMsg{ID: wid, Kind: it.Event.Kind, Text: it.Event.Text, Replay: true})
			} else {
				flush()
				o.emit(ctx, ui.WorkerEventMsg{ID: wid, Kind: it.Event.Kind, Text: it.Event.Text})
			}
		case it.Usage != nil:
			o.mu.Lock()
			o.dshOut[wid] = it.Usage.OutputTokens
			o.mu.Unlock()
			if it.Replay {
				pending = append(pending, ui.WorkerUsageMsg{ID: wid, Input: it.Usage.InputTokens, CacheRead: it.Usage.CacheReadTokens, Output: it.Usage.OutputTokens, Ctx: it.Ctx})
			} else {
				flush()
				o.emit(ctx, ui.WorkerUsageMsg{ID: wid, Input: it.Usage.InputTokens, CacheRead: it.Usage.CacheReadTokens, Output: it.Usage.OutputTokens, Ctx: it.Ctx})
			}
		case it.TurnEnded:
			endReason = it.EndReason
		}
	}
	tail := dshacp.TailSession(ctx, root, session, 0)
	for {
		select {
		case it, ok := <-tail:
			if !ok {
				flush()
				goto done
			}
			apply(it)
			if it.Replay {
				// Re-arm the 200ms quiet window; a live item flushes
				// the batch itself, so only replay items matter here.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(replayFlushDelay)
			}
		case <-timer.C:
			flush()
		}
	}
done:
	if ctx.Err() != nil || endReason == "" {
		return
	}
	// Give the owning delegate a moment to record the finished event.
	grace := o.DshRecoverGrace
	if grace == 0 {
		grace = 10 * time.Second
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(grace):
	}
	o.mu.Lock()
	orphaned := o.unfinished[wid]
	delete(o.unfinished, wid)
	o.mu.Unlock()
	if orphaned {
		status := "done"
		if endReason != "completed" {
			status = "failed"
		}
		o.emit(ctx, ui.WorkerUpsertMsg{ID: wid, Status: status,
			Summary: "(recovered — delegate process gone; turn " + endReason + ")"})
	}
}

// clientFor returns an API client for a worker's model config.
func (o *Orchestrator) clientFor(model string) *opencode.Client {
	if mc, ok := o.modelConfig(model); ok {
		return &opencode.Client{Base: strings.TrimRight(mc.Endpoint, "/")}
	}
	return nil
}

// rateTracker turns cumulative per-worker output counts into the tok/s the
// models panel reports. The counts advance in lumps — one usage event per
// model step — so each delta is measured over the time since the counter
// last advanced, and the last rate is held between advances: dividing a
// step's whole output by the ~2s poll interval reported a 60s step of
// 2000 tokens as ~1000 tok/s, which no local endpoint can do. A worker's
// FIRST sample only seeds its baseline: crediting it would report the
// worker's entire history as one interval's output — a dsh tailer
// attaching to a session already in flight did exactly that (~1700 tok/s).
type rateState struct {
	out  int
	at   time.Time // when out last advanced
	rate float64   // held between advances
}

type rateTracker struct{ last map[string]rateState }

func newRateTracker() *rateTracker { return &rateTracker{last: map[string]rateState{}} }

// observe records a sample of a worker's cumulative output and returns its
// current tok/s: zero on first sight (attaching mid-session must not
// credit the whole history), zero after a backwards jump (a restarted
// worker or a reset session re-seeds the baseline), the held rate while
// the count sits still, and delta / since-last-advance when it grows.
func (t *rateTracker) observe(worker string, out int, now time.Time) float64 {
	st := t.last[worker]
	switch {
	case st.at.IsZero(), out < st.out:
		t.last[worker] = rateState{out: out, at: now}
		return 0
	case out == st.out:
		return st.rate
	default:
		d := now.Sub(st.at)
		if d <= 0 {
			return st.rate
		}
		st.rate = float64(out-st.out) / d.Seconds()
		st.out, st.at = out, now
		t.last[worker] = st
		return st.rate
	}
}

// retain drops state for workers that are no longer open, so the map stays
// bounded over a long run and a reused id re-seeds rather than spiking.
func (t *rateTracker) retain(open map[string]bool) {
	for w := range t.last {
		if !open[w] {
			delete(t.last, w)
		}
	}
}

// pollWorkers refreshes worker usage, per-model endpoint stats (active
// count, tok/s from output deltas), and reconciles workers orphaned by a
// dead delegate process. All local HTTP — zero supervisor tokens.
func (o *Orchestrator) pollWorkers(ctx context.Context) {
	const interval = 2 * time.Second
	rates := newRateTracker()
	// Per-endpoint LLM probe cache (dsh): reachability plus the served
	// model list, refreshed at most every 15s — a configured model the
	// server does not serve must show as "not loaded", not healthy idle.
	type llmProbe struct {
		reachable bool
		models    map[string]int // id → context length; nil = list unknown
		at        time.Time
	}
	llmProbes := map[string]*llmProbe{}
	// Models named by workers but absent from the model set, each logged
	// once. With hot reload this is now the brief window before the
	// watcher picks up an edit, or a model removed while its worker runs.
	unknownModels := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		now := time.Now()

		type snapshot struct{ worker, session, model, dir string }
		o.mu.Lock()
		var open, openDsh []snapshot
		var strangers []string
		for wid := range o.unfinished {
			s := snapshot{wid, o.workerSession[wid], o.workerModel[wid], o.workerDir[wid]}
			mc, known := o.modelConfig(s.model)
			switch {
			case !known:
				// models.toml gained this entry after the TUI last loaded it
				// (delegate re-reads the file per call; the TUI catches up on
				// its next reload poll), or lost it while the worker ran.
				// There is no endpoint to poll: the row still opens and
				// closes from the registry, just without live usage. Treating
				// it as an opencode worker dereferenced a nil client and took
				// the whole TUI down (docs/NOTES.md).
				if !unknownModels[s.model] {
					unknownModels[s.model] = true
					strangers = append(strangers, s.model)
				}
			case mc.Harness == "dsh":
				openDsh = append(openDsh, s)
			default:
				open = append(open, s)
			}
		}
		dshOut := make(map[string]int, len(o.dshOut))
		for k, v := range o.dshOut {
			dshOut[k] = v
		}
		o.mu.Unlock()
		for _, name := range strangers {
			o.emit(ctx, ui.RawLogMsg{Source: "app", Line: "worker model " + name +
				" is not in the loaded models.toml — no live stats for its workers until it is"})
		}

		// The status endpoint is project-scoped: query each endpoint once
		// per distinct worker directory (plus bare, for reachability).
		busyByEndpoint := map[string]map[string]opencode.SessionStatus{}
		reachable := map[string]bool{}
		for _, base := range o.endpoints() {
			c := &opencode.Client{Base: base}
			if _, err := c.Status(ctx, ""); err != nil {
				continue
			}
			reachable[base] = true
			merged := map[string]opencode.SessionStatus{}
			dirs := map[string]bool{}
			for _, s := range open {
				if wc := o.clientFor(s.model); wc == nil || wc.Base != base || dirs[s.dir] {
					continue
				}
				dirs[s.dir] = true
				if st, err := c.Status(ctx, s.dir); err == nil {
					for k, v := range st {
						merged[k] = v
					}
				}
			}
			busyByEndpoint[base] = merged
		}

		activePerModel := map[string]int{}
		ratePerModel := map[string]float64{}
		for _, s := range open {
			c := o.clientFor(s.model)
			if c == nil {
				continue
			}
			info, err := c.SessionInfo(ctx, s.session)
			if err != nil {
				continue
			}
			in := info.Tokens.Input + info.Tokens.Cache.Write
			out := info.Tokens.Output + info.Tokens.Reasoning
			o.emit(ctx, ui.WorkerUsageMsg{ID: s.worker, Input: in, CacheRead: info.Tokens.Cache.Read, Output: out})
			ratePerModel[s.model] += rates.observe(s.worker, out, now)

			busy := busyByEndpoint[c.Base]
			if st, ok := busy[s.session]; ok && st.Type != "idle" {
				activePerModel[s.model]++
			} else if reachable[c.Base] {
				// Idle but no finished event: the delegate process is gone
				// (crash/kill). Classify from the transcript and close the row.
				h := &opencode.Harness{Client: c, Dir: s.dir}
				st, err := h.Status(ctx, s.session)
				if err == nil && (st == harness.StatusDone || st == harness.StatusFailed) {
					o.mu.Lock()
					delete(o.unfinished, s.worker)
					o.mu.Unlock()
					o.emit(ctx, ui.WorkerUpsertMsg{ID: s.worker, Status: string(st),
						Summary: "(recovered — delegate process gone)"})
				}
			}
		}

		// dsh workers have no server to poll: usage flows from their
		// session-log tailers; active = unfinished; tok/s from tailer
		// output rates.
		for _, s := range openDsh {
			activePerModel[s.model]++
			ratePerModel[s.model] += rates.observe(s.worker, dshOut[s.worker], now)
		}
		// Baselines only for workers still open; a finished one must not
		// leave a stale count behind for an id that comes back.
		stillOpen := make(map[string]bool, len(open)+len(openDsh))
		for _, s := range open {
			stillOpen[s.worker] = true
		}
		for _, s := range openDsh {
			stillOpen[s.worker] = true
		}
		rates.retain(stillOpen)

		// Probe every distinct dsh endpoint, idle or not (throttled by
		// the cache above). One snapshot for both loops: a reload between
		// them must not report stats for a set that was never probed.
		modelSet := o.Models()
		for _, mc := range modelSet {
			if mc.Harness != "dsh" {
				continue
			}
			base := strings.TrimRight(mc.Endpoint, "/")
			if p := llmProbes[base]; p != nil && time.Since(p.at) < 15*time.Second {
				continue
			}
			models, ok := llmModels(ctx, base)
			llmProbes[base] = &llmProbe{reachable: ok, models: models, at: time.Now()}
		}

		for _, mc := range modelSet {
			base := strings.TrimRight(mc.Endpoint, "/")
			note := mc.Harness
			ctxWin := 0
			if mc.Harness == "dsh" {
				if p := llmProbes[base]; p != nil {
					switch {
					case !p.reachable:
						note = "endpoint unreachable"
					case p.models == nil: // reachable, list unknown
					default:
						win, served := p.models[mc.Model]
						if !served {
							note = "model not loaded"
						} else {
							ctxWin = win
						}
					}
				}
			} else if !reachable[base] {
				note = "endpoint unreachable"
			}
			o.emit(ctx, ui.ModelStatMsg{
				Name:          mc.Name,
				TokSec:        ratePerModel[mc.Name],
				Active:        activePerModel[mc.Name],
				Note:          note,
				ContextWindow: ctxWin,
			})
		}
	}
}

// llmModels lists an OpenAI-compatible endpoint's served models with
// their context lengths (sglang max_model_len; 0 when unreported),
// dialing IPv4-first like the worker proxy (stale AAAA records must not
// report a working endpoint as unreachable). A reachable endpoint with an
// unparseable listing returns (nil, true): reachable, list unknown.
func llmModels(ctx context.Context, base string) (map[string]int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, false
	}
	resp, err := dshacp.Transport.RoundTrip(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, false
	}
	var parsed struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || len(parsed.Data) == 0 {
		return nil, true
	}
	set := make(map[string]int, len(parsed.Data))
	for _, m := range parsed.Data {
		set[m.ID] = m.MaxModelLen
	}
	return set, true
}

// subscribeWorkerEvents streams one endpoint's /global/event feed into
// worker transcript msgs, reconnecting with backoff on drops.
func (o *Orchestrator) subscribeWorkerEvents(ctx context.Context, base string) {
	c := &opencode.Client{Base: base}
	for {
		ch, err := c.Events(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		for ev := range ch {
			o.emit(ctx, o.mapWorkerEvent(ev)...)
		}
		if ctx.Err() != nil {
			return
		}
		o.emitAsync(ui.RawLogMsg{Source: "wrk", Line: "event stream dropped (" + base + ") — reconnecting"})
	}
}

// mapWorkerEvent turns one opencode SSE event into transcript msgs for the
// worker it belongs to (unknown sessions — e.g. the user's own opencode TUI
// sessions — are ignored).
func (o *Orchestrator) mapWorkerEvent(ev opencode.ServerEvent) []tea.Msg {
	o.mu.Lock()
	wid, known := o.sessionToWorker[ev.Properties.SessionID]
	o.mu.Unlock()
	if !known {
		return nil
	}
	switch ev.Type {
	case "message.part.delta":
		if ev.Properties.Field != "text" && ev.Properties.Field != "reasoning" {
			return nil
		}
		return []tea.Msg{ui.WorkerEventMsg{ID: wid, Kind: ev.Properties.Field, Text: ev.Properties.Delta}}
	case "message.part.updated":
		p := ev.Properties.Part
		if p == nil || p.Type != "tool" {
			return nil
		}
		if p.State.Status != "completed" && p.State.Status != "error" && p.State.Status != "running" {
			return nil
		}
		kind := "tool"
		if p.State.Status == "error" {
			kind = "error"
		}
		return []tea.Msg{ui.WorkerEventMsg{ID: wid, Kind: kind,
			Text: fmt.Sprintf("%s %s [%s]", p.Tool, p.State.Title, p.State.Status)}}
	}
	return nil
}
