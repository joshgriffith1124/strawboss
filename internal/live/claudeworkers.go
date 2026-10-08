package live

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/harness/claudecode"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// tailClaudeWorker streams a Claude Code worker's log (written by the
// claude harness, internal/harness/claudecode) into transcript and usage
// msgs — the same role tailDshWorker plays for dsh. The worker id the
// registry records as its session names the log. History already in the
// log is batched like the dsh tailer's, so a restart doesn't scroll an
// old transcript through the tile. Ends at the log's done record.
func (o *Orchestrator) tailClaudeWorker(ctx context.Context, wid, session string) {
	defer func() {
		o.mu.Lock()
		delete(o.tailing, wid)
		o.mu.Unlock()
	}()
	var pending []tea.Msg
	flush := func() {
		if len(pending) > 0 {
			o.emit(ctx, ui.BatchMsg{Msgs: pending})
			pending = nil
		}
	}
	send := func(replay bool, m tea.Msg) {
		if replay {
			pending = append(pending, m)
			return
		}
		flush()
		o.emit(ctx, m)
	}
	timer := time.NewTimer(replayFlushDelay)
	defer timer.Stop()
	tail := claudecode.Tail(ctx, claudecode.LogPath(o.StateDir, session))
	for {
		select {
		case it, ok := <-tail:
			if !ok {
				flush()
				return
			}
			switch {
			case it.Event != nil:
				send(it.Replay, ui.WorkerEventMsg{ID: wid, Kind: it.Event.Kind, Text: it.Event.Text, Replay: it.Replay})
			case it.Usage != nil:
				// Output tokens drive the per-model tok/s like a dsh
				// worker's (pollWorkers reads dshOut for tailed workers).
				o.mu.Lock()
				o.dshOut[wid] = it.Usage.OutputTokens
				o.mu.Unlock()
				send(it.Replay, ui.WorkerUsageMsg{ID: wid, Input: it.Usage.InputTokens, CacheRead: it.Usage.CacheReadTokens, Output: it.Usage.OutputTokens, Ctx: it.Ctx})
			}
			if it.Replay {
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
}
