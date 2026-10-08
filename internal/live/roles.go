package live

import (
	"fmt"
	"strings"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

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
