// Package router decides which worker model a delegated task runs on: it
// scores a task's weight from its text, maps the score onto the capability
// tiers in models.toml, and routes around models that are unavailable. It
// is pure logic — no I/O — so the delegate command injects availability.
package router

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

// Auto is the --model value that asks strawboss to route each task itself,
// instead of pinning a specific model by name.
const Auto = "auto"

// pathRe matches tokens ending in a common code/doc extension, so a task's
// mentioned files can be counted without parsing prose.
var pathRe = regexp.MustCompile(`[\w./-]+\.(?:go|py|js|ts|tsx|jsx|rs|rb|java|c|h|cpp|md|toml|yaml|yml|json|sh|html|css|sql)\b`)

// Heavy/light work markers, matched case-insensitively as substrings of the
// task text. Order is the signal order.
var (
	heavyKeywords = []string{
		"refactor", "redesign", "architecture", "debug", "deadlock",
		"race condition", "concurren", "migrat", "new package", "implement",
		"integrate", "investigate", "root cause", "optimiz", "performance",
		"security", "parser", "protocol", "algorithm",
	}
	// Deliberately absent: "report" and "comment" — nearly every task ends
	// with "report the output" or mentions comment style, heavy ones
	// included, so they would drag everything down a tier.
	lightKeywords = []string{
		"rename", "typo", "gofmt", "lint", "stub", "bump", "docstring",
		"do not edit", "do not modify", "read-only", "git status", "git log",
	}
)

// Score rates how heavy a task is from its text alone. It returns the score
// (never negative) and the short human-readable signals that produced it,
// e.g. []string{"212 words", "3 files", "refactor"}.
func Score(task string) (int, []string) {
	score := 0
	var signals []string

	// Task length: more words, more work for the model to hold.
	switch w := len(strings.Fields(task)); {
	case w >= 350:
		score += 3
		signals = append(signals, fmt.Sprintf("%d words", w))
	case w >= 150:
		score += 2
		signals = append(signals, fmt.Sprintf("%d words", w))
	case w >= 60:
		score += 1
		signals = append(signals, fmt.Sprintf("%d words", w))
	}

	// Distinct files touched: more files means wider blast radius.
	distinct := map[string]bool{}
	for _, p := range pathRe.FindAllString(task, -1) {
		distinct[p] = true
	}
	switch n := len(distinct); {
	case n >= 3:
		score += 2
		signals = append(signals, fmt.Sprintf("%d files", n))
	case n == 2:
		score += 1
		signals = append(signals, fmt.Sprintf("%d files", n))
	}

	// Heavy work signals: each distinct keyword adds 1, capped at +3.
	lower := strings.ToLower(task)
	added := 0
	for _, kw := range heavyKeywords {
		if added == 3 {
			break
		}
		if strings.Contains(lower, kw) {
			added++
			score++
			signals = append(signals, kw)
		}
	}

	// Light work signals: each distinct keyword subtracts 1, capped at -2.
	subtracted := 0
	for _, kw := range lightKeywords {
		if subtracted == 2 {
			break
		}
		if strings.Contains(lower, kw) {
			subtracted++
			score--
			signals = append(signals, "-"+kw)
		}
	}

	if score < 0 {
		score = 0
	}
	return score, signals
}

// Pick returns the model a task should START on, plus a short reason. A task
// starts at the tier its score maps to, so cheap work stays on cheap models
// and failures escalate rather than mis-routed work burning a big model.
func Pick(models []config.ModelConfig, task string) (config.ModelConfig, string) {
	if len(models) == 0 {
		return config.ModelConfig{}, "no models"
	}
	if !config.Tiered(models) {
		return models[0], "untiered: first entry"
	}

	// Distinct tiers, ascending: the score climbs this ladder.
	set := map[int]bool{}
	for _, mc := range models {
		set[mc.Tier] = true
	}
	tiers := make([]int, 0, len(set))
	for tier := range set {
		tiers = append(tiers, tier)
	}
	sort.Ints(tiers)

	score, signals := Score(task)
	idx := score / 2
	if idx >= len(tiers) {
		idx = len(tiers) - 1
	}
	tier := tiers[idx]

	for _, mc := range models {
		if mc.Tier == tier {
			reason := fmt.Sprintf("score %d → tier %d", score, tier)
			if len(signals) > 0 {
				reason += " (" + strings.Join(signals, ", ") + ")"
			}
			return mc, reason
		}
	}
	// Unreachable: every tier came from the slice itself.
	return models[0], "untiered: first entry"
}
