package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// RoleConfig is one worker role from roles.toml: a focus the supervisor
// can give a task (architect, coder, reviewer, ...). A role is prompt and
// policy only — it runs on a model config like any task, so the harness
// boundary and "model configs, not hosts" both hold.
type RoleConfig struct {
	Name string `toml:"-"`
	// Description is the one-line routing hint shown to the supervisor next
	// to the role name, like a model's description.
	Description string `toml:"description"`
	// Prompt is the preamble prepended to every task run in this role.
	// PromptFile names a file holding it instead (relative paths resolve
	// against roles.toml's directory); LoadRoles reads it into Prompt.
	Prompt     string `toml:"prompt"`
	PromptFile string `toml:"prompt_file"`
	// Model or Tier picks the role's default model when the task does not
	// name one with --model. Model wins when both are set; neither leaves
	// the task on auto routing.
	Model string `toml:"model"`
	Tier  int    `toml:"tier"`
	// ReadOnly roles report and must not modify the working tree: a
	// read-only worker that changed files is failed and its edits are
	// restored from the before snapshot.
	ReadOnly bool `toml:"read_only"`
}

// PipelineConfig chains roles into one delegation: the steps run in order
// inside the delegate process, each step's full reply handed to the next,
// and the supervisor reads a single terse result for the whole chain.
type PipelineConfig struct {
	Name        string   `toml:"-"`
	Description string   `toml:"description"`
	Steps       []string `toml:"steps"`
	// FixRounds lets the final step — which must be a read-only reviewer —
	// send its findings back: the last writing step runs again on them,
	// then the reviewer re-checks, at most this many times. 0 = review
	// only, never loop.
	FixRounds int `toml:"fix_rounds"`
}

// Roles is everything roles.toml defines, in file order.
type Roles struct {
	Roles     []RoleConfig
	Pipelines []PipelineConfig
}

// Role returns the named role.
func (r Roles) Role(name string) (RoleConfig, bool) {
	for _, rc := range r.Roles {
		if rc.Name == name {
			return rc, true
		}
	}
	return RoleConfig{}, false
}

// Pipeline returns the named pipeline.
func (r Roles) Pipeline(name string) (PipelineConfig, bool) {
	for _, pc := range r.Pipelines {
		if pc.Name == name {
			return pc, true
		}
	}
	return PipelineConfig{}, false
}

type rolesFile struct {
	Roles     map[string]RoleConfig     `toml:"roles"`
	Pipelines map[string]PipelineConfig `toml:"pipelines"`
}

// LoadRoles reads roles.toml at path, validating it against the loaded
// model configs. Unlike LoadModels, a missing file is NOT an error: roles
// are optional, and without the file delegation works exactly as before.
func LoadRoles(path string, models []ModelConfig) (Roles, error) {
	var rf rolesFile
	meta, err := toml.DecodeFile(path, &rf)
	if errors.Is(err, fs.ErrNotExist) {
		return Roles{}, nil
	}
	if err != nil {
		return Roles{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		return Roles{}, fmt.Errorf("loading %s: unknown key %q", path, undec[0].String())
	}

	// meta.Keys is in file order; the table headers pick out each entry's
	// first appearance, so the supervisor sees roles in the order written.
	var out Roles
	seen := map[string]bool{}
	for _, key := range meta.Keys() {
		if len(key) < 2 || seen[key[0]+"."+key[1]] {
			continue
		}
		seen[key[0]+"."+key[1]] = true
		switch key[0] {
		case "roles":
			rc := rf.Roles[key[1]]
			rc.Name = key[1]
			out.Roles = append(out.Roles, rc)
		case "pipelines":
			pc := rf.Pipelines[key[1]]
			pc.Name = key[1]
			out.Pipelines = append(out.Pipelines, pc)
		}
	}

	tiers := map[int]bool{}
	names := map[string]bool{}
	for _, mc := range models {
		tiers[mc.Tier] = true
		names[mc.Name] = true
	}
	for i, rc := range out.Roles {
		if rc.PromptFile != "" {
			p := rc.PromptFile
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return Roles{}, fmt.Errorf("role %q: prompt_file: %w", rc.Name, err)
			}
			out.Roles[i].Prompt = strings.TrimSpace(string(b))
		}
		if rc.Model != "" && !names[rc.Model] {
			return Roles{}, fmt.Errorf("role %q: model %q is not in models.toml", rc.Name, rc.Model)
		}
		if rc.Model == "" && rc.Tier != 0 && !tiers[rc.Tier] {
			return Roles{}, fmt.Errorf("role %q: no model in models.toml has tier %d", rc.Name, rc.Tier)
		}
	}
	for _, pc := range out.Pipelines {
		if len(pc.Steps) == 0 {
			return Roles{}, fmt.Errorf("pipeline %q: no steps", pc.Name)
		}
		writes := false
		for _, step := range pc.Steps {
			rc, ok := out.Role(step)
			if !ok {
				return Roles{}, fmt.Errorf("pipeline %q: step %q is not a role in %s", pc.Name, step, path)
			}
			writes = writes || !rc.ReadOnly
		}
		if pc.FixRounds < 0 {
			return Roles{}, fmt.Errorf("pipeline %q: fix_rounds %d must be 0 or more", pc.Name, pc.FixRounds)
		}
		if pc.FixRounds > 0 {
			last, _ := out.Role(pc.Steps[len(pc.Steps)-1])
			if !last.ReadOnly || !writes {
				return Roles{}, fmt.Errorf("pipeline %q: fix_rounds needs a read-only last step reviewing an earlier writing step", pc.Name)
			}
		}
	}
	return out, nil
}

// TierModel is the first unmetered model config (file order) in the given
// tier — the starting point routing then resolves against availability
// and free slots like any pinned task. A role reaches a metered model only
// by naming it with `model`.
func TierModel(models []ModelConfig, tier int) (ModelConfig, bool) {
	for _, mc := range models {
		if mc.Tier == tier && !mc.Metered() {
			return mc, true
		}
	}
	return ModelConfig{}, false
}
