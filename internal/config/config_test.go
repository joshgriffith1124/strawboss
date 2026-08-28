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
			name: "two models sorted by name",
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
				if models[0].Name != "a-small" || models[1].Name != "qwen-coder" {
					t.Errorf("order = %s, %s", models[0].Name, models[1].Name)
				}
				if models[0].Harness != "opencode" {
					t.Errorf("default harness = %q, want opencode", models[0].Harness)
				}
				if models[1].Endpoint != "http://gx10a:4096" {
					t.Errorf("Endpoint = %q", models[1].Endpoint)
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

func TestLoadModelsMissingFile(t *testing.T) {
	if _, err := LoadModels(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("want error for missing models.toml")
	}
}
