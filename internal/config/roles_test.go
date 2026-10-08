package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRoles(t *testing.T) {
	models := []ModelConfig{{Name: "fast", Tier: 1}, {Name: "big", Tier: 2}}
	tests := []struct {
		name    string
		toml    string
		wantErr string
		check   func(t *testing.T, r Roles)
	}{
		{
			name: "roles and pipelines in file order",
			toml: `
[roles.reviewer]
description = "finds bugs"
read_only = true
tier = 2

[roles.coder]
prompt = "write code"

[pipelines.fix]
steps = ["coder", "reviewer"]
fix_rounds = 1
`,
			check: func(t *testing.T, r Roles) {
				if len(r.Roles) != 2 || r.Roles[0].Name != "reviewer" || r.Roles[1].Name != "coder" {
					t.Fatalf("roles = %+v, want reviewer then coder", r.Roles)
				}
				if !r.Roles[0].ReadOnly || r.Roles[0].Tier != 2 || r.Roles[1].Prompt != "write code" {
					t.Errorf("fields not parsed: %+v", r.Roles)
				}
				p, ok := r.Pipeline("fix")
				if !ok || p.FixRounds != 1 || strings.Join(p.Steps, ",") != "coder,reviewer" {
					t.Errorf("pipeline = %+v", p)
				}
			},
		},
		{name: "unknown model", toml: "[roles.a]\nmodel = \"nope\"\n", wantErr: `model "nope" is not in models.toml`},
		{name: "unknown tier", toml: "[roles.a]\ntier = 3\n", wantErr: "no model in models.toml has tier 3"},
		{name: "unknown key", toml: "[roles.a]\nreadonly = true\n", wantErr: "unknown key"},
		{name: "pipeline step not a role", toml: "[roles.a]\n[pipelines.p]\nsteps = [\"a\", \"b\"]\n", wantErr: `step "b" is not a role`},
		{name: "empty pipeline", toml: "[pipelines.p]\nsteps = []\n", wantErr: "no steps"},
		{
			name:    "fix rounds need a read-only step after a writer",
			toml:    "[roles.coder]\n[roles.other]\n[pipelines.p]\nsteps = [\"coder\", \"other\"]\nfix_rounds = 1\n",
			wantErr: "fix_rounds needs a read-only step",
		},
		{
			name:    "a read-only step before any writer is not a gate",
			toml:    "[roles.r]\nread_only = true\n[roles.coder]\n[pipelines.p]\nsteps = [\"r\", \"coder\"]\nfix_rounds = 1\n",
			wantErr: "fix_rounds needs a read-only step",
		},
		{
			name: "a gate in the middle is enough",
			toml: "[roles.coder]\n[roles.security]\nread_only = true\n[roles.docs]\n[pipelines.p]\nsteps = [\"coder\", \"security\", \"docs\"]\nfix_rounds = 1\n",
			check: func(t *testing.T, r Roles) {
				if p, ok := r.Pipeline("p"); !ok || p.FixRounds != 1 {
					t.Errorf("pipeline = %+v", p)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "roles.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := LoadRoles(path, models)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, r)
		})
	}
}

// TestLoadRolesMissingFile: roles are optional — no file, no roles, no error.
func TestLoadRolesMissingFile(t *testing.T) {
	r, err := LoadRoles(filepath.Join(t.TempDir(), "roles.toml"), nil)
	if err != nil || len(r.Roles) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("LoadRoles(missing) = %+v, %v; want empty, nil", r, err)
	}
}

// TestLoadRolesPromptFile: prompt_file resolves next to roles.toml.
func TestLoadRolesPromptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reviewer.md"), []byte("  Review carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "roles.toml")
	if err := os.WriteFile(path, []byte("[roles.reviewer]\nprompt_file = \"reviewer.md\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRoles(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Roles[0].Prompt; got != "Review carefully." {
		t.Errorf("prompt = %q, want the trimmed file contents", got)
	}
}

func TestLoadRolesMode(t *testing.T) {
	tests := []struct {
		toml    string
		on      bool
		wantErr bool
	}{
		{"", false, false},
		{"[roles]\nmode = \"off\"\n", false, false},
		{"[roles]\nmode = \"on\"\n", true, false},
		{"[roles]\nmode = \"pipeline\"\n", false, true},
	}
	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(tt.toml), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if (err != nil) != tt.wantErr {
			t.Fatalf("Load(%q) err = %v, wantErr %v", tt.toml, err, tt.wantErr)
		}
		if err == nil && cfg.Roles.On() != tt.on {
			t.Errorf("Load(%q).Roles.On() = %v, want %v", tt.toml, cfg.Roles.On(), tt.on)
		}
	}
}
