package router

import (
	"strings"
	"testing"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestScore(t *testing.T) {
	bigWords := strings.Repeat("word ", 400)

	cases := []struct {
		name        string
		task        string
		wantScore   int
		wantSignals []string
		notSignals  []string
	}{
		{
			name:      "short task",
			task:      "rename the variable foo to bar in main.go",
			wantScore: 0,
		},
		{
			name:        "three distinct files plus heavy keyword",
			task:        "refactor a.go, b.go and c.go",
			wantScore:   3,
			wantSignals: []string{"3 files", "refactor"},
		},
		{
			name:        "four hundred words",
			task:        bigWords,
			wantScore:   3,
			wantSignals: []string{"400 words"},
		},
		{
			name:        "same file twice counts once",
			task:        "edit a.go and a.go, plus b.go",
			wantScore:   1,
			wantSignals: []string{"2 files"},
		},
		{
			name:      "light keywords never push below zero",
			task:      "rename it, fix a typo, add a comment",
			wantScore: 0,
			// "-2 total" cap: the third light keyword is not counted.
			wantSignals: []string{"-rename", "-typo"},
			notSignals:  []string{"-comment"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score, signals := Score(tc.task)
			if score != tc.wantScore {
				t.Fatalf("Score = %d (signals %v), want %d", score, signals, tc.wantScore)
			}
			for _, s := range tc.wantSignals {
				if !containsString(signals, s) {
					t.Errorf("signals %v missing %q", signals, s)
				}
			}
			for _, s := range tc.notSignals {
				if containsString(signals, s) {
					t.Errorf("signals %v should not contain %q", signals, s)
				}
			}
		})
	}
}

func TestPick(t *testing.T) {
	untiered := []config.ModelConfig{
		{Name: "first"},
		{Name: "second"},
	}
	twoTiers := []config.ModelConfig{
		{Name: "big", Tier: 2},
		{Name: "fast", Tier: 1},
	}
	threeTiers := []config.ModelConfig{
		{Name: "t1", Tier: 1},
		{Name: "t2", Tier: 2},
		{Name: "t3", Tier: 3},
	}
	heavyTask := "refactor and debug a.go, b.go, c.go"
	hugeTask := strings.Repeat("word ", 400) + " refactor a.go b.go c.go debug"
	midTask := "debug a.go and b.go"

	cases := []struct {
		name       string
		models     []config.ModelConfig
		task       string
		wantName   string
		wantReason string // exact reason, when set
		reasonSub  string // substring that must appear in the reason
	}{
		{
			name:       "untiered list returns first entry",
			models:     untiered,
			task:       "anything",
			wantName:   "first",
			wantReason: "untiered: first entry",
		},
		{
			name:      "tiny task lands on the lowest tier",
			models:    twoTiers,
			task:      "rename x to y",
			wantName:  "fast",
			reasonSub: "tier",
		},
		{
			name:     "heavy task lands on the highest tier",
			models:   twoTiers,
			task:     heavyTask,
			wantName: "big",
		},
		{
			name:     "mid task lands on the middle tier",
			models:   threeTiers,
			task:     midTask,
			wantName: "t2",
		},
		{
			name:     "huge score clamps to the top tier",
			models:   threeTiers,
			task:     hugeTask,
			wantName: "t3",
		},
		{
			name:       "empty models returns zero value",
			models:     nil,
			task:       "anything",
			wantName:   "",
			wantReason: "no models",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc, reason := Pick(tc.models, tc.task)
			if mc.Name != tc.wantName {
				t.Fatalf("Pick name = %q, want %q (reason %q)", mc.Name, tc.wantName, reason)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if tc.reasonSub != "" && !strings.Contains(reason, tc.reasonSub) {
				t.Errorf("reason %q does not contain %q", reason, tc.reasonSub)
			}
		})
	}
}
