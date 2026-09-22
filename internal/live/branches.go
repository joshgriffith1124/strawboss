package live

import (
	"github.com/joshgriffith1124/strawboss/internal/ui"
	"github.com/joshgriffith1124/strawboss/internal/worktree"
)

// Branch review (ui hooks, the `b` overlay). `delegate --worktree` leaves
// each worker's work on a strawboss/* branch and stops; these let the
// operator list, inspect, merge or discard them from the TUI. All of it is
// local git against the project the supervisor runs in — zero supervisor
// tokens: the supervisor is not told, this is operator control like kill
// and retry.

// repoDir is the project the supervisor (and so every delegation) runs in.
func (o *Orchestrator) repoDir() string {
	if o.Driver != nil && o.Driver.Dir != "" {
		return o.Driver.Dir
	}
	return "."
}

// ListBranches lists worker branches awaiting review. A git failure is a
// toast and an empty list, never a crash.
func (o *Orchestrator) ListBranches() []ui.BranchInfo {
	bs, err := worktree.ListBranches(o.repoDir())
	if err != nil {
		o.emitAsync(ui.ToastMsg{Text: "branches: " + err.Error()})
		return nil
	}
	out := make([]ui.BranchInfo, len(bs))
	for i, b := range bs {
		out[i] = ui.BranchInfo{Name: b.Name, Ahead: b.Ahead, Files: b.Files,
			Added: b.Added, Deleted: b.Deleted, Subject: b.Subject, When: b.When}
	}
	return out
}

// BranchDiff returns what a worker branch changed since it forked.
func (o *Orchestrator) BranchDiff(name string) (string, error) {
	return worktree.Diff(o.repoDir(), name, 0)
}

// MergeBranch merges a worker branch into the project's current branch and
// cleans up its worktree; conflicts abort and leave everything as it was.
func (o *Orchestrator) MergeBranch(name string) error {
	if err := worktree.Merge(o.repoDir(), name); err != nil {
		return err
	}
	o.emitAsync(ui.RawLogMsg{Source: "app", Line: "merged " + name + " into the project's current branch"})
	return nil
}

// DiscardBranch deletes a worker branch and its worktree, work and all.
func (o *Orchestrator) DiscardBranch(name string) error {
	if err := worktree.Discard(o.repoDir(), name); err != nil {
		return err
	}
	o.emitAsync(ui.RawLogMsg{Source: "app", Line: "discarded " + name})
	return nil
}
