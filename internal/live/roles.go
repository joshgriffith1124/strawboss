package live

import (
	"fmt"
	"strings"
	"time"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// SetRolesMode flips the roles mode at runtime (the TUI toggle): the
// supervisor prompt is rebuilt, and since a running supervisor keeps the
// prompt it started with, the supervisor restarts on the same session to
// pick it up — at once when idle, after the current turn otherwise. The
// restart re-reads the conversation without its prompt cache, once.
func (o *Orchestrator) SetRolesMode(on bool) {
	o.RolesOn.Store(on)
	o.emitAsync(ui.RolesModeMsg{On: on})
	state := "off"
	if on {
		state = "on"
	}
	if o.PromptFor == nil || o.Driver == nil {
		// A configured system_prompt replaces the built-in one entirely:
		// there is no roles section to add or drop.
		o.emitAsync(ui.ToastMsg{Text: "roles mode " + state + " — delegate flags only (config sets a custom system_prompt)"})
		return
	}
	if on && o.RolesPath != "" {
		// Say so when there is nothing to offer: the mode would otherwise
		// look on while the prompt silently lacks the section.
		roles, err := config.LoadRoles(o.RolesPath, o.Models())
		switch {
		case err != nil:
			o.emitAsync(ui.ToastMsg{Text: "roles mode on, but " + err.Error()})
		case len(roles.Roles) == 0:
			o.emitAsync(ui.ToastMsg{Text: "roles mode on, but there is no roles.toml — see examples/roles.toml"})
		}
	}
	o.Driver.SetSystemPrompt(o.PromptFor(o.Models()))

	o.mu.Lock()
	stream, busy := o.stream, o.turnBusy
	if busy {
		o.promptStale = true
	} else {
		o.stream = nil
	}
	o.mu.Unlock()
	switch {
	case stream == nil:
		o.emitAsync(ui.ToastMsg{Text: "roles mode " + state})
	case busy:
		o.emitAsync(ui.ToastMsg{Text: "roles mode " + state + " — applies when the current turn ends"})
	default:
		go stream.Shutdown(3 * time.Second)
		o.emitAsync(ui.ToastMsg{Text: "roles mode " + state + " — supervisor restarts on your next prompt"})
	}
}

// RolesPrompt is the supervisor-prompt section for the roles mode
// (config.toml [roles] mode = "on"): roles.toml's roles and pipelines with
// their descriptions, and when to reach for them. Appended to
// BuildSystemPrompt; "" when roles.toml defines nothing, so turning the
// mode on without a roles file changes nothing.
func RolesPrompt(exe string, roles config.Roles) string {
	if len(roles.Roles) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nROLES MODE is on: tasks can run in a role, or through a pipeline of roles.\n")
	b.WriteString("Roles (--role <name>, sticky like --model; a role picks its own default model unless you pass --model):\n")
	for _, rc := range roles.Roles {
		line := "  - " + rc.Name
		if rc.Description != "" {
			line += ": " + rc.Description
		}
		if rc.ReadOnly {
			line += " (read-only: reports, never edits)"
		}
		b.WriteString(line + "\n")
	}
	if len(roles.Pipelines) > 0 {
		b.WriteString("Pipelines (--pipeline <name>): the task runs through each step in order inside one delegate call — every step's full report is handed to the next, and you read ONE terse result for the whole chain:\n")
		for _, p := range roles.Pipelines {
			line := fmt.Sprintf("  - %s: %s", p.Name, strings.Join(p.Steps, " → "))
			if p.FixRounds > 0 {
				line += fmt.Sprintf(" (up to %d fix round(s) when the last step requests changes)", p.FixRounds)
			}
			if p.Description != "" {
				line += " — " + p.Description
			}
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "  %s delegate --pipeline <name> --task \"<the whole feature, self-contained>\"\n", exe)
	}
	b.WriteString("Use a pipeline for a non-trivial feature or fix where a plan or a review pays for itself. Keep small chores, renames, reports and one-file edits as plain delegations (or a single role) — every pipeline step is a full worker run. Do not hand-chain roles across separate delegate calls when a pipeline fits: each hop through you costs your context.")
	return b.String()
}
