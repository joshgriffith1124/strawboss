package live

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// modelsPollEvery is how often the watcher stats models.toml. A stat is
// cheap and edits are rare; polling avoids a file-notification dependency
// and survives editors that replace the file instead of writing it.
var modelsPollEvery = 3 * time.Second

// watchModels follows ModelsPath and swaps in the new model set whenever
// the file changes. The delegate command re-reads models.toml on every
// call, so without this the TUI and delegate disagree about which models
// exist until a restart — the gap that crashed the TUI on 2026-09-21
// (docs/NOTES.md).
//
// What follows an edit, and when:
//   - poller, Models pane, kill/retry, server management: next pass;
//   - opencode event subscriptions: new endpoints subscribed at once;
//   - the supervisor's prompt: its next spawn. A running supervisor keeps
//     the roster it started with, since restarting it just for this would
//     cost its prompt cache. Delegate routes and checks availability
//     itself, so a stale roster only means the supervisor won't name a
//     new model until then; auto routing already reaches it.
//
// A file that fails to parse keeps the previous set and says why, once per
// broken version: a half-saved edit must never empty the Models pane.
func (o *Orchestrator) watchModels(ctx context.Context) {
	if o.ModelsPath == "" {
		return
	}
	type stamp struct {
		mod  time.Time
		size int64
	}
	var last stamp
	if fi, err := os.Stat(o.ModelsPath); err == nil {
		last = stamp{fi.ModTime(), fi.Size()}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(modelsPollEvery):
		}
		fi, err := os.Stat(o.ModelsPath)
		if err != nil {
			continue // mid-replace, or deleted: keep what we have
		}
		cur := stamp{fi.ModTime(), fi.Size()}
		if cur == last {
			continue
		}
		last = cur
		o.reloadModels(ctx)
	}
}

// reloadModels loads ModelsPath and, when it parses, makes it current.
func (o *Orchestrator) reloadModels(ctx context.Context) {
	next, err := config.LoadModels(o.ModelsPath)
	if err != nil {
		o.emit(ctx, ui.ToastMsg{Text: "models.toml not reloaded: " + err.Error()})
		return
	}
	prev := o.Models()
	o.models.Store(&next)

	added, removed := modelDiff(prev, next)
	names := make([]string, len(next))
	for i, mc := range next {
		names[i] = mc.Name
	}
	o.emit(ctx, ui.ModelSetMsg{Names: names})
	o.subscribeNewEndpoints(ctx)
	if o.PromptFor != nil && o.Driver != nil {
		o.Driver.SetSystemPrompt(o.PromptFor(next))
	}

	line := "models.toml reloaded"
	if len(added) > 0 {
		line += " · added " + strings.Join(added, ", ")
	}
	if len(removed) > 0 {
		line += " · removed " + strings.Join(removed, ", ")
	}
	if len(added) == 0 && len(removed) == 0 {
		line += " · settings changed"
	}
	line += " (delegate uses it now; the supervisor's model list updates on its next restart)"
	o.emit(ctx, ui.RawLogMsg{Source: "app", Line: line})
}

// modelDiff names the models present in only one of two sets, each in its
// set's file order.
func modelDiff(prev, next []config.ModelConfig) (added, removed []string) {
	in := func(set []config.ModelConfig, name string) bool {
		for _, mc := range set {
			if mc.Name == name {
				return true
			}
		}
		return false
	}
	for _, mc := range next {
		if !in(prev, mc.Name) {
			added = append(added, mc.Name)
		}
	}
	for _, mc := range prev {
		if !in(next, mc.Name) {
			removed = append(removed, mc.Name)
		}
	}
	return added, removed
}
