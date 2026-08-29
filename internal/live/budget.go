package live

import (
	"fmt"
	"os"
	"path/filepath"

	"strawboss/internal/ui"
)

// The budget guard watches the metered side of a run — notional
// supervisor cost and the 5h plan window — and blocks NEW delegations
// past a ceiling by writing a per-project stop marker that the delegate
// command refuses on. The refusal is a terse tool result the supervisor
// reads, so stopping costs almost nothing; warnings go to the human
// (toast + push), never into supervisor context.

// BudgetStopFile is the per-project marker that blocks new delegations.
// It contains the human-readable reason.
func BudgetStopFile(stateDir, dir string) string {
	return filepath.Join(ProjectDir(stateDir, dir), "budget-stop")
}

// noteBudgetUsage feeds the guard from the supervisor stream (called by
// observeSup with the orchestrator's own accumulation).
func (o *Orchestrator) noteBudgetUsage(costDelta, fiveHour float64) {
	b := o.Budget
	if b.MaxCostUSD <= 0 && b.MaxPlan5h <= 0 {
		return
	}
	o.mu.Lock()
	o.supCostTotal += costDelta
	if fiveHour > 0 {
		o.fiveHourNow = fiveHour
	}
	cost, five := o.supCostTotal, o.fiveHourNow
	warned, stopped := o.budgetWarned, o.budgetStopped
	o.mu.Unlock()

	overCost := b.MaxCostUSD > 0 && cost >= b.MaxCostUSD
	overWin := b.MaxPlan5h > 0 && five*100 >= b.MaxPlan5h
	nearCost := b.MaxCostUSD > 0 && cost >= 0.8*b.MaxCostUSD
	nearWin := b.MaxPlan5h > 0 && five*100 >= 0.8*b.MaxPlan5h

	switch {
	case (overCost || overWin) && !stopped:
		reason := fmt.Sprintf("notional cost $%.2f reached the $%.2f ceiling", cost, b.MaxCostUSD)
		if overWin {
			reason = fmt.Sprintf("5h plan window at %.0f%% reached the %.0f%% ceiling", five*100, b.MaxPlan5h)
		}
		o.mu.Lock()
		o.budgetStopped = true
		o.mu.Unlock()
		stop := BudgetStopFile(o.StateDir, o.Driver.Dir)
		_ = os.MkdirAll(filepath.Dir(stop), 0o755)
		_ = os.WriteFile(stop, []byte(reason+"\n"), 0o644)
		o.emitAsync(ui.ToastMsg{Text: "budget ceiling hit — new delegations blocked (" + reason + ")"})
		o.notifyText("strawboss: budget ceiling hit", "new delegations blocked — "+reason)
	case stopped && !overCost && !overWin:
		// Only the plan window recovers on its own; cost never shrinks,
		// so reaching here means a window-based stop has lifted.
		o.mu.Lock()
		o.budgetStopped = false
		o.mu.Unlock()
		_ = os.Remove(BudgetStopFile(o.StateDir, o.Driver.Dir))
		o.emitAsync(ui.ToastMsg{Text: "plan window recovered — delegations unblocked"})
		o.notifyText("strawboss: delegations unblocked", "the plan window recovered below the ceiling")
	case (nearCost || nearWin) && !warned && !stopped:
		o.mu.Lock()
		o.budgetWarned = true
		o.mu.Unlock()
		text := fmt.Sprintf("budget at 80%%: cost $%.2f of $%.2f · 5h window %.0f%% of %.0f%%",
			cost, b.MaxCostUSD, five*100, b.MaxPlan5h)
		o.emitAsync(ui.ToastMsg{Text: text})
		o.notifyText("strawboss: budget warning", text)
	}
}
