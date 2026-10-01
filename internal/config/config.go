// Package config loads strawboss configuration from TOML files under the
// state directory (default ~/.strawboss): config.toml for app settings and
// models.toml for the named model configs workers bind to.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// ToolsMode selects the dsh tool transport: "native" (default),
	// "code" (one run_code tool + a generated TypeScript SDK prompt), or
	// "both". Ignored by the opencode harness.
	ToolsMode string `toml:"tools_mode"`
	// MaxTokens caps a dsh worker's per-request output (shared with
	// reasoning). Default 49152 — parity with the opencode limit; an
	// undersized cap makes big tasks grind to an output-budget failure
	// (docs/NOTES.md). Ignored by the opencode harness (opencode.json
	// limit.output owns it there).
	MaxTokens int `toml:"max_tokens"`
	// Temperature, when set, is forced onto every chat-completions request
	// a dsh worker makes (the worker's LLM proxy rewrites the body). Local
	// servers otherwise fall back to the model's generation_config — GLM's
	// is 1.0, where it emits stray CJK tokens into code (docs/NOTES.md).
	// Unset = leave the request alone. Ignored by the opencode harness.
	Temperature *float64 `toml:"temperature"`
	// ReasoningEffort sets a dsh worker's reasoning_effort for this model
	// (off|low|high|max), overriding the global STRAWBOSS_DSH_REASONING.
	// GLM does not think unless asked (docs/NOTES.md). Unset = the global
	// env var, else off. Ignored by the opencode harness.
	ReasoningEffort string `toml:"reasoning_effort"`
	// Tier ranks capability: 1 is the cheapest/fastest, higher is stronger
	// (and slower). The supervisor is told to pick the lowest tier that fits
	// a task, and a failed task escalates to the next tier up rather than
	// the next file entry. 0 = unset: a file where no entry sets a tier is
	// untiered and keeps plain file-order preference; once any entry sets
	// one, LoadModels fills the unset ones with 1.
	Tier int `toml:"tier"`
	// Description is a one-line routing hint shown to the supervisor next
	// to the model name — what the model is good for and how big a task it
	// can carry. It is the supervisor's only basis for choosing a tier, so
	// keep it concrete ("single-function edits, renames, test stubs").
	Description string `toml:"description"`
	// MaxConcurrent caps how many workers run on this model at once,
	// across every delegate process and TUI retry; the rest wait as
	// "queued" until a slot frees. For a single-GPU server whose parallel
	// requests thrash rather than overlap. 0 = unlimited.
	MaxConcurrent int `toml:"max_concurrent"`
	// Timeout, when set, is the per-attempt worker deadline for this
	// model: each run (first try and an escalation retry alike) gets a
	// fresh context bounded by it. 0 = unset: the delegate command falls
	// back to its own default; an explicit --timeout flag overrides it.
	Timeout time.Duration `toml:"-"`
	// TimeoutRaw is the on-disk `timeout` value (a Go duration string,
	// e.g. "40m"); LoadModels parses and validates it into Timeout.
	TimeoutRaw string `toml:"timeout"`
}

// Tiered reports whether models use capability tiers (any entry sets one).
func Tiered(models []ModelConfig) bool {
	for _, mc := range models {
		if mc.Tier > 0 {
			return true
		}
	}
	return false
}

// Supervisor holds settings for spawning the claude CLI.
type Supervisor struct {
	// Command is the claude binary to spawn. Default "claude".
	Command string `toml:"command"`
	// PermissionMode passed as --permission-mode. Default "dontAsk" so the
	// supervisor never blocks on an interactive prompt (CLAUDE.md invariant 6).
	PermissionMode string `toml:"permission_mode"`
	// AllowedTools are appended to the built-in --allowedTools baseline
	// (delegate + Read/Edit/Write/Glob). Extras only — the baseline is
	// never replaced, so the delegate pattern can't be lost to a config
	// edit.
	AllowedTools []string `toml:"allowed_tools"`
	// SystemPrompt optionally appended via --append-system-prompt.
	SystemPrompt string `toml:"system_prompt"`
}

// Notify configures optional push notifications. The bell stays; these
// add remote reach — all local HTTP/CLI from the TUI, zero supervisor
// tokens (the OpenClaw two-way path injects prompts the same way typing
// does, which is user input, not observability overhead).
type Notify struct {
	// NtfyTopic enables worker-failure pushes to <server>/<topic>.
	NtfyTopic string `toml:"ntfy_topic"`
	// NtfyServer overrides the default https://ntfy.sh.
	NtfyServer string `toml:"ntfy_server"`

	// OpenClawTarget enables notifications through an OpenClaw gateway
	// channel (e.g. Discord): the `--target` value openclaw expects,
	// such as "channel:<id>". Empty disables the OpenClaw path.
	OpenClawTarget string `toml:"openclaw_target"`
	// OpenClawChannel is the chat channel type. Default "discord".
	OpenClawChannel string `toml:"openclaw_channel"`
	// OpenClawBin overrides the openclaw CLI path. Default "openclaw".
	OpenClawBin string `toml:"openclaw_bin"`
	// OpenClawTwoWay also polls the channel: messages you send there are
	// injected into the supervisor as prompts (mid-turn works), and the
	// supervisor's replies are relayed back — remote unblocking when
	// strawboss is stuck and you are away. Bot-authored messages are
	// ignored, so notifications never feed back.
	OpenClawTwoWay bool `toml:"openclaw_two_way"`
}

// Budget guards the metered side of a run: the supervisor's notional API
// cost and the plan window. Workers are free and never limited. Crossing
// 80% of a ceiling warns (toast + push); crossing the ceiling blocks new
// delegations — the delegate command refuses with advice the supervisor
// reads, so the stop costs almost nothing in supervisor tokens.
type Budget struct {
	// MaxCostUSD is the per-run notional supervisor cost ceiling. 0 = off.
	MaxCostUSD float64 `toml:"max_cost_usd"`
	// MaxPlan5h is the 5-hour plan-window utilization ceiling in percent
	// (e.g. 80). The block lifts automatically when the window recovers.
	// 0 = off.
	MaxPlan5h float64 `toml:"max_plan_5h"`
}

// Config is the app configuration from config.toml.
type Config struct {
	Supervisor Supervisor `toml:"supervisor"`
	Notify     Notify     `toml:"notify"`
	Budget     Budget     `toml:"budget"`

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
// to favor earlier entries), so list the preferred model first. Entries
// that set `tier` override that with a capability ladder (see
// ModelConfig.Tier); file order then only breaks ties within a tier. Unlike
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
		switch mc.ToolsMode {
		case "", "native", "code", "both":
		default:
			return nil, fmt.Errorf("model %q: unknown tools_mode %q (native, code, or both)", name, mc.ToolsMode)
		}
		if mc.Temperature != nil && (*mc.Temperature < 0 || *mc.Temperature > 2) {
			return nil, fmt.Errorf("model %q: temperature %v out of range (0–2)", name, *mc.Temperature)
		}
		switch mc.ReasoningEffort {
		case "", "off", "low", "high", "max":
		default:
			return nil, fmt.Errorf("model %q: unknown reasoning_effort %q (off, low, high, or max)", name, mc.ReasoningEffort)
		}
		if mc.Tier < 0 {
			return nil, fmt.Errorf("model %q: tier %d must be 1 or higher", name, mc.Tier)
		}
		if mc.MaxConcurrent < 0 {
			return nil, fmt.Errorf("model %q: max_concurrent %d must be 0 (unlimited) or more", name, mc.MaxConcurrent)
		}
		if mc.TimeoutRaw != "" {
			d, err := time.ParseDuration(mc.TimeoutRaw)
			if err != nil {
				return nil, fmt.Errorf("model %q: timeout %q: %w", name, mc.TimeoutRaw, err)
			}
			if d < 0 {
				return nil, fmt.Errorf("model %q: timeout %q must not be negative", name, mc.TimeoutRaw)
			}
			mc.Timeout = d
		}
		if strings.ContainsAny(mc.Description, "\r\n") {
			return nil, fmt.Errorf("model %q: description must be a single line", name)
		}
		out = append(out, mc)
	}
	// A partly tiered file is still tiered: entries that skipped the key
	// sit on the bottom rung rather than outside the ladder.
	if Tiered(out) {
		for i := range out {
			if out[i].Tier == 0 {
				out[i].Tier = 1
			}
		}
	}
	return out, nil
}
