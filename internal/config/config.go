// Package config loads strawboss configuration from TOML files under the
// state directory (default ~/.strawboss): config.toml for app settings and
// models.toml for the named model configs workers bind to.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ModelConfig is a named inference target a worker can bind to. Workers and
// the UI reference models by Name only — hardware/hosts stay invisible.
//
// Endpoint's meaning depends on the harness: for opencode it is the
// `opencode serve` base URL; for dsh it is the OpenAI-compatible LLM base
// URL the worker talks to directly.
type ModelConfig struct {
	Name     string `toml:"-"`
	Endpoint string `toml:"endpoint"`
	Model    string `toml:"model"`
	Harness  string `toml:"harness"`
	// Variant selects the opencode model variant (e.g. a non-thinking mode
	// for reasoning models). Empty uses the provider default.
	Variant string `toml:"variant"`
	// APIKey is sent to the endpoint by harnesses that talk to the LLM
	// directly (dsh). Local OpenAI-compatible servers accept any value;
	// empty defaults to "local".
	APIKey string `toml:"api_key"`
}

// Supervisor holds settings for spawning the claude CLI.
type Supervisor struct {
	// Command is the claude binary to spawn. Default "claude".
	Command string `toml:"command"`
	// PermissionMode passed as --permission-mode. Default "dontAsk" so the
	// supervisor never blocks on an interactive prompt (CLAUDE.md invariant 6).
	PermissionMode string `toml:"permission_mode"`
	// AllowedTools passed as --allowedTools; must cover everything the
	// supervisor is expected to do, the delegate command above all.
	AllowedTools []string `toml:"allowed_tools"`
	// SystemPrompt optionally appended via --append-system-prompt.
	SystemPrompt string `toml:"system_prompt"`
}

// Config is the app configuration from config.toml.
type Config struct {
	Supervisor Supervisor `toml:"supervisor"`

	// StateDir is where logs and session state live. Default ~/.strawboss.
	StateDir string `toml:"state_dir"`
}

// DefaultStateDir returns ~/.strawboss (or an error if home is unknown).
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".strawboss"), nil
}

func defaults() Config {
	return Config{
		Supervisor: Supervisor{
			Command:        "claude",
			PermissionMode: "dontAsk",
		},
	}
}

// Load reads config.toml at path. A missing file is not an error: defaults
// are returned so strawboss runs with zero configuration.
func Load(path string) (Config, error) {
	cfg := defaults()
	meta, err := toml.DecodeFile(path, &cfg)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		return Config{}, fmt.Errorf("loading %s: unknown key %q", path, undec[0].String())
	}
	if cfg.StateDir == "" {
		cfg.StateDir, err = DefaultStateDir()
		if err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// modelsFile is the on-disk shape of models.toml:
//
//	[models.qwen-coder]
//	endpoint = "http://gx10a:4096"
//	model    = "qwen2.5-coder-32b"
//	harness  = "opencode"
type modelsFile struct {
	Models map[string]ModelConfig `toml:"models"`
}

// knownHarnesses gates the harness field: opencode (v1) and dsh (DeepSeek
// Harness ACP workers, docs/NOTES.md).
var knownHarnesses = map[string]bool{"opencode": true, "dsh": true}

// LoadModels reads models.toml at path and returns configs in declaration
// order — the file's order is the preference order (the supervisor is told
// to favor earlier entries), so list the preferred model first. Unlike
// Load, a missing file IS an error: there is nothing to delegate to
// without model configs.
func LoadModels(path string) ([]ModelConfig, error) {
	var mf modelsFile
	meta, err := toml.DecodeFile(path, &mf)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		return nil, fmt.Errorf("loading %s: unknown key %q", path, undec[0].String())
	}
	if len(mf.Models) == 0 {
		return nil, fmt.Errorf("loading %s: no [models.<name>] entries", path)
	}

	// meta.Keys is in file order; the [models.<name>] table headers pick
	// out each entry's first appearance.
	names := make([]string, 0, len(mf.Models))
	seen := map[string]bool{}
	for _, key := range meta.Keys() {
		if len(key) < 2 || key[0] != "models" || seen[key[1]] {
			continue
		}
		seen[key[1]] = true
		names = append(names, key[1])
	}

	out := make([]ModelConfig, 0, len(names))
	for _, name := range names {
		mc := mf.Models[name]
		mc.Name = name
		if mc.Harness == "" {
			mc.Harness = "opencode"
		}
		if mc.Endpoint == "" {
			return nil, fmt.Errorf("model %q: endpoint is required", name)
		}
		if mc.Model == "" {
			return nil, fmt.Errorf("model %q: model is required", name)
		}
		if !knownHarnesses[mc.Harness] {
			return nil, fmt.Errorf("model %q: unknown harness %q", name, mc.Harness)
		}
		out = append(out, mc)
	}
	return out, nil
}
