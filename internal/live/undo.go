package live

import (
	"strings"

	"github.com/joshgriffith1124/strawboss/internal/snapshot"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// UndoWorker reverts a finished worker's edits in its working directory,
// undoing every path that changed between its before/after snapshots. This
// is operator control: zero supervisor tokens — the supervisor is not told,
// like kill, retry, and branch review. A dir of "" falls back to the
// project directory.
func (o *Orchestrator) UndoWorker(id, dir string) ([]string, error) {
	if dir == "" {
		dir = o.repoDir()
	}
	files, err := snapshot.Restore(dir, id)
	if err != nil {
		return nil, err
	}
	if len(files) > 0 {
		o.emitAsync(ui.RawLogMsg{Source: "app", Line: "undid " + id + ": reverted " + strings.Join(files, ", ")})
	}
	return files, nil
}
