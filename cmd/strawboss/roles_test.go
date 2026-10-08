package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/config"
	"github.com/joshgriffith1124/strawboss/internal/harness"
	"github.com/joshgriffith1124/strawboss/internal/router"
	"github.com/joshgriffith1124/strawboss/internal/runner"
)

func TestStickyAt(t *testing.T) {
	pins := []modelPin{{name: "coder", from: 0}, {name: "none", from: 2}, {name: "reviewer", from: 3}}
	tests := []struct {
		pins []modelPin
		i    int
		want string
	}{
		{nil, 0, ""},
		{pins, 0, "coder"},
		{pins, 1, "coder"},
		{pins, 2, ""}, // "none" clears
		{pins, 3, "reviewer"},
		{[]modelPin{{name: "coder", from: 1}}, 0, "coder"}, // a task written before the flag binds to it
	}
	for _, tt := range tests {
		if got := stickyAt(tt.pins, tt.i); got != tt.want {
			t.Errorf("stickyAt(%v, %d) = %q, want %q", tt.pins, tt.i, got, tt.want)
		}
	}
}

func TestRoleWant(t *testing.T) {
	models := []config.ModelConfig{{Name: "fast", Tier: 1}, {Name: "big-a", Tier: 2}, {Name: "big-b", Tier: 2}}
	tests := []struct {
		rc   config.RoleConfig
		want string
	}{
		{config.RoleConfig{Model: "big-b", Tier: 1}, "big-b"}, // model wins over tier
		{config.RoleConfig{Tier: 2}, "big-a"},                 // first in the tier
		{config.RoleConfig{}, router.Auto},
	}
	for _, tt := range tests {
		if got := roleWant(tt.rc, models); got != tt.want {
			t.Errorf("roleWant(%+v) = %q, want %q", tt.rc, got, tt.want)
		}
	}
}

func TestRolePrompt(t *testing.T) {
	p := rolePrompt(config.RoleConfig{Name: "reviewer", Prompt: "Find bugs.", ReadOnly: true}, "check the parser")
	for _, want := range []string{`"reviewer" role`, "Find bugs.", "READ-ONLY", "Task:\ncheck the parser"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if p := rolePrompt(config.RoleConfig{Name: "coder"}, "x"); strings.Contains(p, "READ-ONLY") {
		t.Errorf("writing role told it is read-only:\n%s", p)
	}
}

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		report, want string
	}{
		{"looks fine\nVERDICT: PASS", "PASS"},
		{"1. off-by-one\n\n**VERDICT: CHANGES**\n", "CHANGES"},
		{"verdict: changes — see above", "CHANGES"},
		{"VERDICT: PASS\nVERDICT: CHANGES", "CHANGES"},                  // the last verdict line wins
		{"I'd say VERDICT: PASS mid-line\nVERDICT: CHANGES", "CHANGES"}, // only a line that starts with it counts
		{"no verdict here", ""},
		{"VERDICT: maybe", ""},
	}
	for _, tt := range tests {
		if got := parseVerdict(tt.report); got != tt.want {
			t.Errorf("parseVerdict(%q) = %q, want %q", tt.report, got, tt.want)
		}
	}
}

func TestStepPrompt(t *testing.T) {
	p := config.PipelineConfig{Name: "feature", Steps: []string{"architect", "coder", "reviewer"}}
	got := stepPrompt(p, 1, "add a flag", []stepResult{{role: "architect", worker: "w1", report: "plan: touch main.go"}}, "")
	for _, want := range []string{"step 2 of 3", "architect → coder → reviewer", "add a flag", "### architect (w1)", "plan: touch main.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("step prompt missing %q:\n%s", want, got)
		}
	}
	long := strings.Repeat("x", maxHandoffBytes+100)
	if got := stepPrompt(p, 1, "t", []stepResult{{role: "architect", worker: "w1", report: long}}, ""); !strings.Contains(got, "…(cut)") {
		t.Error("an oversized report was not cut")
	}
}

// TestRunPipeline drives the chain with scripted steps: order and handoff,
// a failing step stopping the chain, and the reviewer's fix rounds.
func TestRunPipeline(t *testing.T) {
	roles := config.Roles{Roles: []config.RoleConfig{
		{Name: "architect", ReadOnly: true},
		{Name: "coder"},
		{Name: "reviewer", ReadOnly: true},
	}}
	tests := []struct {
		name       string
		pipe       config.PipelineConfig
		replies    map[string][]string // role -> reports, one per run; "FAIL" fails the step
		wantStatus harness.Status
		wantRuns   string // roles in run order
		wantInHead []string
	}{
		{
			name:       "plan, code, review: all pass",
			pipe:       config.PipelineConfig{Name: "feature", Steps: []string{"architect", "coder", "reviewer"}},
			replies:    map[string][]string{"architect": {"the plan"}, "coder": {"done"}, "reviewer": {"ok"}},
			wantStatus: harness.StatusDone,
			wantRuns:   "architect coder reviewer",
			wantInHead: []string{"pipeline feature:", "architect w1 ✓ → coder w2 ✓ → reviewer w3 ✓"},
		},
		{
			name:       "a failed step stops the chain",
			pipe:       config.PipelineConfig{Name: "feature", Steps: []string{"architect", "coder", "reviewer"}},
			replies:    map[string][]string{"architect": {"the plan"}, "coder": {"FAIL"}},
			wantStatus: harness.StatusFailed,
			wantRuns:   "architect coder",
			wantInHead: []string{"coder w2 ✗", "stopped at coder"},
		},
		{
			name:       "changes requested, one fix round, then pass",
			pipe:       config.PipelineConfig{Name: "fix", Steps: []string{"coder", "reviewer"}, FixRounds: 2},
			replies:    map[string][]string{"coder": {"v1", "v2"}, "reviewer": {"bug\nVERDICT: CHANGES", "VERDICT: PASS"}},
			wantStatus: harness.StatusDone,
			wantRuns:   "coder reviewer coder reviewer",
			wantInHead: []string{"1 fix round(s)", "verdict PASS"},
		},
		{
			name:       "fix rounds exhausted: still changes",
			pipe:       config.PipelineConfig{Name: "fix", Steps: []string{"coder", "reviewer"}, FixRounds: 1},
			replies:    map[string][]string{"coder": {"v1", "v2"}, "reviewer": {"VERDICT: CHANGES", "VERDICT: CHANGES"}},
			wantStatus: harness.StatusFailed,
			wantRuns:   "coder reviewer coder reviewer",
			wantInHead: []string{"reviewer still requests changes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs []string
			used := map[string]int{}
			var prompts []string
			env := pipelineEnv{roles: roles, step: func(_ context.Context, rc config.RoleConfig, prompt, _, _, _ string) runner.Outcome {
				runs = append(runs, rc.Name)
				prompts = append(prompts, prompt)
				reply := tt.replies[rc.Name][used[rc.Name]]
				used[rc.Name]++
				id := fmt.Sprintf("w%d", len(runs))
				if reply == "FAIL" {
					return runner.Outcome{WorkerID: id, Res: harness.Result{Status: harness.StatusFailed, Summary: "broke"}}
				}
				return runner.Outcome{WorkerID: id, Res: harness.Result{Status: harness.StatusDone, Summary: reply, Report: reply}}
			}}
			oc := runPipeline(context.Background(), env, tt.pipe, "the task")
			if got := strings.Join(runs, " "); got != tt.wantRuns {
				t.Errorf("runs = %q, want %q", got, tt.wantRuns)
			}
			if oc.Res.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s\n%s", oc.Res.Status, tt.wantStatus, oc.Res.Summary)
			}
			head := strings.SplitN(oc.Res.Summary, "\n", 2)[0]
			for _, want := range tt.wantInHead {
				if !strings.Contains(head, want) {
					t.Errorf("summary head %q missing %q", head, want)
				}
			}
			// Handoff: every step after the first sees the first step's report.
			for i := 1; i < len(prompts); i++ {
				first := tt.replies[runs[0]][0]
				if !strings.Contains(prompts[i], first) {
					t.Errorf("step %d prompt lacks the first report %q", i+1, first)
				}
			}
		})
	}
}

// TestRunPipelineFixPrompt: a fix round hands the reviewer's findings to
// the writing step, and the reviewer is told the verdict rule.
func TestRunPipelineFixPrompt(t *testing.T) {
	roles := config.Roles{Roles: []config.RoleConfig{{Name: "coder"}, {Name: "reviewer", ReadOnly: true}}}
	var prompts []string
	replies := []string{"v1", "nil deref in parse()\nVERDICT: CHANGES", "v2", "VERDICT: PASS"}
	var labels []string
	env := pipelineEnv{roles: roles, step: func(_ context.Context, _ config.RoleConfig, prompt, _, pipeline, label string) runner.Outcome {
		prompts = append(prompts, prompt)
		labels = append(labels, pipeline+" "+label)
		r := replies[len(prompts)-1]
		return runner.Outcome{WorkerID: fmt.Sprintf("w%d", len(prompts)), Res: harness.Result{Status: harness.StatusDone, Report: r}}
	}}
	runPipeline(context.Background(), env, config.PipelineConfig{Name: "fix", Steps: []string{"coder", "reviewer"}, FixRounds: 1}, "t")
	if len(prompts) != 4 {
		t.Fatalf("ran %d steps, want 4", len(prompts))
	}
	if !strings.Contains(prompts[1], "VERDICT: PASS") {
		t.Error("reviewer was not told the verdict rule")
	}
	if !strings.Contains(prompts[2], "Fix round 1 of 1") || !strings.Contains(prompts[2], "nil deref in parse()") {
		t.Errorf("fix prompt lacks the findings:\n%s", prompts[2])
	}
	// Each worker is labelled with its place in the chain.
	if want := "fix 1/2|fix 2/2|fix fix 1 · reviewer|fix 2/2 recheck 1"; strings.Join(labels, "|") != want {
		t.Errorf("labels = %q, want %q", strings.Join(labels, "|"), want)
	}
}

// TestRunPipelineGates: every read-only step after a writer is a gate with
// its own fix-round budget — a security gate in the middle sends its
// findings back to the coder before the reviewer runs; an architect
// before any writer only plans.
func TestRunPipelineGates(t *testing.T) {
	roles := config.Roles{Roles: []config.RoleConfig{
		{Name: "architect", ReadOnly: true},
		{Name: "coder"},
		{Name: "security", ReadOnly: true},
		{Name: "reviewer", ReadOnly: true},
	}}
	tests := []struct {
		name       string
		replies    map[string][]string
		wantRuns   string
		wantStatus harness.Status
		wantInHead []string
	}{
		{
			name: "security finds a hole, coder fixes, then the reviewer passes",
			replies: map[string][]string{
				"architect": {"plan"},
				"coder":     {"v1", "v2"},
				"security":  {"SQL injection in q()\nVERDICT: CHANGES", "VERDICT: PASS"},
				"reviewer":  {"VERDICT: PASS"},
			},
			wantRuns:   "architect coder security coder security reviewer",
			wantStatus: harness.StatusDone,
			wantInHead: []string{"1 fix round(s)", "security PASS", "reviewer PASS"},
		},
		{
			name: "each gate has its own budget",
			replies: map[string][]string{
				"architect": {"plan"},
				"coder":     {"v1", "v2", "v3"},
				"security":  {"VERDICT: CHANGES", "VERDICT: PASS"},
				"reviewer":  {"VERDICT: CHANGES", "VERDICT: PASS"},
			},
			wantRuns:   "architect coder security coder security reviewer coder reviewer",
			wantStatus: harness.StatusDone,
			wantInHead: []string{"2 fix round(s)", "security PASS", "reviewer PASS"},
		},
		{
			name: "a gate still requesting changes stops the chain",
			replies: map[string][]string{
				"architect": {"plan"},
				"coder":     {"v1", "v2"},
				"security":  {"VERDICT: CHANGES", "VERDICT: CHANGES"},
			},
			wantRuns:   "architect coder security coder security",
			wantStatus: harness.StatusFailed,
			wantInHead: []string{"security still requests changes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs []string
			used := map[string]int{}
			env := pipelineEnv{roles: roles, step: func(_ context.Context, rc config.RoleConfig, prompt, _, _, _ string) runner.Outcome {
				runs = append(runs, rc.Name)
				if rc.Name == "architect" && strings.Contains(prompt, "VERDICT") {
					t.Error("the architect (before any writer) was asked for a verdict")
				}
				reply := tt.replies[rc.Name][used[rc.Name]]
				used[rc.Name]++
				return runner.Outcome{WorkerID: fmt.Sprintf("w%d", len(runs)), Res: harness.Result{Status: harness.StatusDone, Report: reply}}
			}}
			p := config.PipelineConfig{Name: "secure", Steps: []string{"architect", "coder", "security", "reviewer"}, FixRounds: 1}
			oc := runPipeline(context.Background(), env, p, "the task")
			if got := strings.Join(runs, " "); got != tt.wantRuns {
				t.Errorf("runs = %q, want %q", got, tt.wantRuns)
			}
			if oc.Res.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s\n%s", oc.Res.Status, tt.wantStatus, oc.Res.Summary)
			}
			head := strings.SplitN(oc.Res.Summary, "\n", 2)[0]
			for _, want := range tt.wantInHead {
				if !strings.Contains(head, want) {
					t.Errorf("summary head %q missing %q", head, want)
				}
			}
		})
	}
}
