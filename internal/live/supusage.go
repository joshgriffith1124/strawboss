package live

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// Supervisor usage is persisted PER RUN so the token economy compares
// like with like: the worker side of the panel replays the whole run
// from the registry, and a supervisor counter that reset on every TUI
// restart made the local offload look better than it is. Persisting also
// lets the budget guard's cost ceiling survive restarts.

// supUsageTotals is the per-run supervisor ledger (one JSON file in the
// project slot, named by run id).
type supUsageTotals struct {
	In         int     `json:"in"`
	Out        int     `json:"out"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	Turns      int     `json:"turns"`
	CostUSD    float64 `json:"cost_usd"`
	// Ctx is the last API call's full prompt size — the session's context
	// footprint. Persisted so a resume can show what the next prompt will
	// re-read BEFORE it burns.
	Ctx int `json:"ctx"`
}

func (o *Orchestrator) supUsagePath(run string) string {
	return filepath.Join(ProjectDir(o.StateDir, o.Driver.Dir), "sup-usage-"+run+".json")
}

func (o *Orchestrator) loadSupUsage(run string) supUsageTotals {
	var t supUsageTotals
	if b, err := os.ReadFile(o.supUsagePath(run)); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	return t
}

// noteSupCtx tracks the latest per-call context footprint in memory;
// recordSupUsage persists it with the ledger at turn end.
func (o *Orchestrator) noteSupCtx(ctx int) {
	if ctx <= 0 {
		return
	}
	o.mu.Lock()
	o.supTotals.Ctx = ctx
	o.mu.Unlock()
}

// recordSupUsage folds one completed turn's usage into the run ledger.
func (o *Orchestrator) recordSupUsage(msg ui.SupUsageMsg) {
	o.mu.Lock()
	run := o.RunID
	o.supTotals.In += msg.Input
	o.supTotals.Out += msg.Output
	o.supTotals.CacheRead += msg.CacheRead
	o.supTotals.CacheWrite += msg.CacheWrite
	o.supTotals.Turns += msg.Turns
	o.supTotals.CostUSD += msg.CostUSD
	t := o.supTotals
	o.mu.Unlock()
	path := o.supUsagePath(run)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if b, err := json.Marshal(t); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
}

// seedSupUsage loads the run's persisted ledger into memory (budget guard
// included) and emits it so the UI starts from the run's real totals, not
// zero. Called at startup and after a session switch.
func (o *Orchestrator) seedSupUsage() {
	o.mu.Lock()
	run := o.RunID
	o.mu.Unlock()
	t := o.loadSupUsage(run)
	o.mu.Lock()
	o.supTotals = t
	o.supCostTotal = t.CostUSD
	o.budgetWarned, o.budgetStopped = false, false
	o.mu.Unlock()
	if t.In+t.Out+t.CacheRead+t.Turns > 0 {
		o.emitAsync(ui.SupUsageMsg{
			Input: t.In, Output: t.Out,
			CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
			CostUSD: t.CostUSD, Turns: t.Turns,
			Ctx: t.Ctx,
		})
	}
}
