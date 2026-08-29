package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"strawboss/internal/harness"
	"strawboss/internal/harness/opencode"
	"strawboss/internal/registry"
	"strawboss/internal/ui"
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
		o.applyRegistryEvent(ctx, ev)
	}
}

func (o *Orchestrator) applyRegistryEvent(ctx context.Context, ev registry.Event) {
	if o.RunID != "" && ev.Run != o.RunID {
		return // another run's worker — not this session's story
	}
	switch ev.Type {
	case "spawned":
		o.mu.Lock()
		o.sessionToWorker[ev.Session] = ev.Worker
		o.workerSession[ev.Worker] = ev.Session
		o.workerModel[ev.Worker] = ev.Model
		o.workerDir[ev.Worker] = ev.Dir
		o.workerTask[ev.Worker] = ev.Task
		o.unfinished[ev.Worker] = true
		o.mu.Unlock()
		o.emit(ctx, ui.WorkerUpsertMsg{
			ID: ev.Worker, Model: ev.Model, Task: ev.Task,
			Status: "running", Started: ev.TS,
		})
	case "finished":
		o.mu.Lock()
		delete(o.unfinished, ev.Worker)
		o.mu.Unlock()
		o.emit(ctx,
			ui.WorkerUpsertMsg{ID: ev.Worker, Status: ev.Status, Summary: ev.Summary, LogPath: ev.LogPath, Ended: ev.TS},
			ui.WorkerUsageMsg{ID: ev.Worker, Input: ev.InputTokens, Output: ev.OutputTokens},
		)
	}
}

// clientFor returns an API client for a worker's model config.
func (o *Orchestrator) clientFor(model string) *opencode.Client {
	for _, mc := range o.Models {
		if mc.Name == model {
			return &opencode.Client{Base: strings.TrimRight(mc.Endpoint, "/")}
		}
	}
	return nil
}

// pollWorkers refreshes worker usage, per-model endpoint stats (active
// count, tok/s from output deltas), and reconciles workers orphaned by a
// dead delegate process. All local HTTP — zero supervisor tokens.
func (o *Orchestrator) pollWorkers(ctx context.Context) {
	const interval = 2 * time.Second
	lastOut := map[string]int{} // worker → last output tokens seen
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		type snapshot struct{ worker, session, model, dir string }
		o.mu.Lock()
		var open []snapshot
		for wid := range o.unfinished {
			open = append(open, snapshot{wid, o.workerSession[wid], o.workerModel[wid], o.workerDir[wid]})
		}
		o.mu.Unlock()

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
				if o.clientFor(s.model).Base != base || dirs[s.dir] {
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
		outDeltaPerModel := map[string]int{}
		for _, s := range open {
			c := o.clientFor(s.model)
			if c == nil {
				continue
			}
			info, err := c.SessionInfo(ctx, s.session)
			if err != nil {
				continue
			}
			in := info.Tokens.Input + info.Tokens.Cache.Read + info.Tokens.Cache.Write
			out := info.Tokens.Output + info.Tokens.Reasoning
			o.emit(ctx, ui.WorkerUsageMsg{ID: s.worker, Input: in, Output: out})
			outDeltaPerModel[s.model] += out - lastOut[s.worker]
			lastOut[s.worker] = out

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

		for _, mc := range o.Models {
			base := strings.TrimRight(mc.Endpoint, "/")
			note := mc.Harness
			if !reachable[base] {
				note = "endpoint unreachable"
			}
			o.emit(ctx, ui.ModelStatMsg{
				Name:   mc.Name,
				TokSec: float64(outDeltaPerModel[mc.Name]) / interval.Seconds(),
				Active: activePerModel[mc.Name],
				Note:   note,
			})
		}
	}
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
// worker it belongs to (unknown sessions — e.g. Josh's own opencode TUI
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
