package live

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/supervisor"
	"github.com/joshgriffith1124/strawboss/internal/ui"
)

// TestSetRolesMode: the toggle rebuilds the supervisor prompt from the
// live mode, reports it to the UI, defers to the end of an in-flight
// turn, and warns when there is no roles.toml to offer.
func TestSetRolesMode(t *testing.T) {
	dir := t.TempDir()
	rolesPath := filepath.Join(dir, "roles.toml")
	if err := os.WriteFile(rolesPath, []byte("[roles.reviewer]\ndescription = \"finds bugs\"\nread_only = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := New(&supervisor.Driver{}, nil, dir)
	o.RolesPath = rolesPath
	o.PromptFor = func(ms []config.ModelConfig) string {
		p := "base"
		if o.RolesOn.Load() {
			roles, _ := config.LoadRoles(rolesPath, ms)
			p += RolesPrompt("strawboss", roles)
		}
		return p
	}
	drain := func() []tea.Msg {
		var got []tea.Msg
		for {
			select {
			case m := <-o.Feed():
				got = append(got, m)
			default:
				return got
			}
		}
	}

	o.SetRolesMode(true)
	if !strings.Contains(o.Driver.SystemPrompt, "ROLES MODE is on") {
		t.Errorf("prompt after on = %q, want the roles section", o.Driver.SystemPrompt)
	}
	var sawMode bool
	for _, m := range drain() {
		if rm, ok := m.(ui.RolesModeMsg); ok && rm.On {
			sawMode = true
		}
	}
	if !sawMode {
		t.Error("no RolesModeMsg{On: true} for the UI")
	}

	o.SetRolesMode(false)
	if o.Driver.SystemPrompt != "base" {
		t.Errorf("prompt after off = %q, want the plain prompt", o.Driver.SystemPrompt)
	}
	drain()

	// Mid-turn: the change waits for the turn to end.
	o.mu.Lock()
	o.turnBusy = true
	o.mu.Unlock()
	o.SetRolesMode(true)
	o.mu.Lock()
	stale := o.promptStale
	o.mu.Unlock()
	if !stale {
		t.Error("a toggle mid-turn did not mark the prompt stale for the turn's end")
	}
	drain()

	// No roles.toml: the mode turns on, and says there is nothing to offer.
	o.RolesPath = filepath.Join(dir, "missing.toml")
	o.SetRolesMode(true)
	var warned bool
	for _, m := range drain() {
		if toast, ok := m.(ui.ToastMsg); ok && strings.Contains(toast.Text, "no roles.toml") {
			warned = true
		}
	}
	if !warned {
		t.Error("turning the mode on without roles.toml did not warn")
	}
}

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
