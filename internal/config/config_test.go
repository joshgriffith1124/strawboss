package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
		check   func(t *testing.T, cfg Config)
	}{
		{
			name: "full config",
			toml: `
state_dir = "/tmp/sb"
[supervisor]
command = "/opt/claude"
permission_mode = "acceptEdits"
allowed_tools = ["Bash(strawboss delegate:*)", "Read"]
`,
			check: func(t *testing.T, cfg Config) {
				if cfg.StateDir != "/tmp/sb" {
					t.Errorf("StateDir = %q", cfg.StateDir)
				}
				if cfg.Supervisor.Command != "/opt/claude" {
					t.Errorf("Command = %q", cfg.Supervisor.Command)
				}
				if cfg.Supervisor.PermissionMode != "acceptEdits" {
					t.Errorf("PermissionMode = %q", cfg.Supervisor.PermissionMode)
				}
				if len(cfg.Supervisor.AllowedTools) != 2 {
					t.Errorf("AllowedTools = %v", cfg.Supervisor.AllowedTools)
				}
			},
		},
		{
			name: "empty file keeps defaults",
			toml: "",
			check: func(t *testing.T, cfg Config) {
				if cfg.Supervisor.Command != "claude" {
					t.Errorf("Command = %q, want claude", cfg.Supervisor.Command)
				}
				if cfg.Supervisor.PermissionMode != "dontAsk" {
					t.Errorf("PermissionMode = %q, want dontAsk", cfg.Supervisor.PermissionMode)
				}
				if cfg.StateDir == "" {
					t.Error("StateDir empty, want default")
				}
			},
		},
		{
			name:    "unknown key rejected",
			toml:    "surpervisor_command = \"claude\"\n",
			wantErr: "unknown key",
		},
		{
			name:    "malformed toml",
			toml:    "[supervisor\n",
			wantErr: "loading",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeFile(t, "config.toml", tt.toml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Supervisor.Command != "claude" {
		t.Errorf("Command = %q, want claude", cfg.Supervisor.Command)
	}
}

func TestLoadModels(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
		check   func(t *testing.T, models []ModelConfig)
	}{
		{
			// Declaration order is preference order — deliberately
			// non-alphabetical here to prove file order wins.
			name: "declaration order preserved",
			toml: `
[models.qwen-coder]
endpoint = "http://gx10a:4096"
model = "qwen2.5-coder-32b"
harness = "opencode"

[models.a-small]
endpoint = "http://gx10b:8080"
model = "qwen2.5-7b"
`,
			check: func(t *testing.T, models []ModelConfig) {
				if len(models) != 2 {
					t.Fatalf("got %d models", len(models))
				}
				if models[0].Name != "qwen-coder" || models[1].Name != "a-small" {
					t.Errorf("order = %s, %s", models[0].Name, models[1].Name)
				}
				if models[1].Harness != "opencode" {
					t.Errorf("default harness = %q, want opencode", models[1].Harness)
				}
				if models[0].Endpoint != "http://gx10a:4096" {
					t.Errorf("Endpoint = %q", models[0].Endpoint)
				}
			},
		},
		{
			name:    "missing endpoint",
			toml:    "[models.x]\nmodel = \"m\"\n",
			wantErr: `model "x": endpoint is required`,
		},
		{
			name:    "missing model",
			toml:    "[models.x]\nendpoint = \"http://h:1\"\n",
			wantErr: `model "x": model is required`,
		},
		{
			name:    "unknown harness",
			toml:    "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nharness = \"qwen-code\"\n",
			wantErr: `unknown harness "qwen-code"`,
		},
		{
			name:    "empty file",
			toml:    "",
			wantErr: "no [models.<name>] entries",
		},
		{
			name:    "unknown key",
			toml:    "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nhost = \"gx10a\"\n",
			wantErr: "unknown key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models, err := LoadModels(writeFile(t, "models.toml", tt.toml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, models)
		})
	}
}

func TestLoadModelsMaxConcurrent(t *testing.T) {
	good := "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nmax_concurrent = 2\n\n[models.y]\nendpoint = \"http://h:1\"\nmodel = \"m\"\n"
	models, err := LoadModels(writeFile(t, "models.toml", good))
	if err != nil {
		t.Fatal(err)
	}
	if models[0].MaxConcurrent != 2 || models[1].MaxConcurrent != 0 {
		t.Errorf("max_concurrent = %d, %d; want 2, 0 (unset = unlimited)", models[0].MaxConcurrent, models[1].MaxConcurrent)
	}
	bad := "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nmax_concurrent = -1\n"
	if _, err := LoadModels(writeFile(t, "bad.toml", bad)); err == nil ||
		!strings.Contains(err.Error(), "max_concurrent") {
		t.Errorf("negative max_concurrent: err = %v", err)
	}
}

func TestLoadModelsTiers(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
		check   func(t *testing.T, models []ModelConfig)
	}{
		{
			name: "untiered",
			toml: `
[models.one]
endpoint = "http://h:1"
model = "m"

[models.two]
endpoint = "http://h:1"
model = "m"
`,
			check: func(t *testing.T, models []ModelConfig) {
				if len(models) != 2 {
					t.Fatalf("got %d models", len(models))
				}
				for i, mc := range models {
					if mc.Tier != 0 {
						t.Errorf("models[%d].Tier = %d, want 0", i, mc.Tier)
					}
				}
				if Tiered(models) {
					t.Error("Tiered = true, want false")
				}
			},
		},
		{
			name: "tiered with description",
			toml: `
[models.fast]
endpoint = "http://h:1"
model = "m"
tier = 1
description = "small edits, renames"

[models.big]
endpoint = "http://h:1"
model = "m"
tier = 2
`,
			check: func(t *testing.T, models []ModelConfig) {
				if len(models) != 2 {
					t.Fatalf("got %d models", len(models))
				}
				fast, big := models[0], models[1]
				if fast.Name != "fast" || big.Name != "big" {
					t.Errorf("order = %s, %s", fast.Name, big.Name)
				}
				if fast.Tier != 1 {
					t.Errorf("fast.Tier = %d, want 1", fast.Tier)
				}
				if fast.Description != "small edits, renames" {
					t.Errorf("fast.Description = %q", fast.Description)
				}
				if big.Tier != 2 {
					t.Errorf("big.Tier = %d, want 2", big.Tier)
				}
				if !Tiered(models) {
					t.Error("Tiered = false, want true")
				}
			},
		},
		{
			name: "unset tier defaults to 1 in a tiered file",
			toml: `
[models.a]
endpoint = "http://h:1"
model = "m"

[models.b]
endpoint = "http://h:1"
model = "m"
tier = 3
`,
			check: func(t *testing.T, models []ModelConfig) {
				if len(models) != 2 {
					t.Fatalf("got %d models", len(models))
				}
				if models[0].Tier != 1 {
					t.Errorf("a.Tier = %d, want 1", models[0].Tier)
				}
				if models[1].Tier != 3 {
					t.Errorf("b.Tier = %d, want 3", models[1].Tier)
				}
			},
		},
		{
			name:    "negative tier",
			toml:    "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\ntier = -1\n",
			wantErr: "must be 1 or higher",
		},
		{
			name: "multiline description",
			toml: `
[models.x]
endpoint = "http://h:1"
model = "m"
description = """
line one
line two
"""
`,
			wantErr: "description must be a single line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models, err := LoadModels(writeFile(t, "models.toml", tt.toml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, models)
		})
	}
}

func TestLoadModelsMissingFile(t *testing.T) {
	if _, err := LoadModels(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("want error for missing models.toml")
	}
}

func TestLoadModelsToolsMode(t *testing.T) {
	good := "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nharness = \"dsh\"\ntools_mode = \"code\"\n"
	models, err := LoadModels(writeFile(t, "models.toml", good))
	if err != nil || models[0].ToolsMode != "code" {
		t.Fatalf("models = %+v err %v", models, err)
	}
	bad := "[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\ntools_mode = \"yolo\"\n"
	if _, err := LoadModels(writeFile(t, "bad.toml", bad)); err == nil ||
		!strings.Contains(err.Error(), "unknown tools_mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestModelTemperature(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "models.toml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	models, err := LoadModels(write("[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\nharness = \"dsh\"\ntemperature = 0.2\n\n[models.y]\nendpoint = \"http://h:1\"\nmodel = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Temperature == nil || *models[0].Temperature != 0.2 {
		t.Errorf("x temperature = %v", models[0].Temperature)
	}
	if models[1].Temperature != nil {
		t.Errorf("y temperature should be unset, got %v", *models[1].Temperature)
	}
	if _, err := LoadModels(write("[models.x]\nendpoint = \"http://h:1\"\nmodel = \"m\"\ntemperature = 3\n")); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Errorf("temperature 3 accepted: %v", err)
	}
}
