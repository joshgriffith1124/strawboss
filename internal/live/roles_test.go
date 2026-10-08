package live

import (
	"strings"
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

func TestRolesPrompt(t *testing.T) {
	if got := RolesPrompt("/bin/strawboss", config.Roles{}); got != "" {
		t.Errorf("no roles: got %q, want empty (the mode changes nothing without roles.toml)", got)
	}
	roles := config.Roles{
		Roles: []config.RoleConfig{
			{Name: "coder", Description: "implements"},
			{Name: "reviewer", Description: "finds bugs", ReadOnly: true},
		},
		Pipelines: []config.PipelineConfig{{Name: "fix", Steps: []string{"coder", "reviewer"}, FixRounds: 1, Description: "code then review"}},
	}
	got := RolesPrompt("/bin/strawboss", roles)
	for _, want := range []string{
		"ROLES MODE is on",
		"- coder: implements",
		"- reviewer: finds bugs (read-only",
		"- fix: coder → reviewer (up to 1 fix round(s)",
		"/bin/strawboss delegate --pipeline <name>",
		"Keep small chores",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("roles prompt missing %q:\n%s", want, got)
		}
	}
}
