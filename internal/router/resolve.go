package router

import (
	"sort"

	"github.com/joshgriffith1124/strawboss/internal/config"
)

// Resolve returns the model a task should actually run on, given the model it
// WANTS and an availability check. ok is false when nothing at all is
// available; want is then returned unchanged so the caller can run it anyway
// and surface the real error.
func Resolve(models []config.ModelConfig, want config.ModelConfig, avail func(config.ModelConfig) bool) (got config.ModelConfig, ok bool) {
	if available(avail, want) {
		return want, true
	}
	if m, found := firstAvailable(candidates(models, want), avail); found {
		return m, true
	}
	return want, false
}

// PreferFree re-routes a saturated pick to the first available same-tier
// model with a free slot, instead of letting the task queue on a model whose
// slots are all busy. got is returned unchanged when it still has a free
// slot (or free is nil), when the list is untiered, or when no sibling is
// both available and free — the task then queues on got as before.
func PreferFree(models []config.ModelConfig, got config.ModelConfig,
	avail func(config.ModelConfig) bool, free func(config.ModelConfig) bool) config.ModelConfig {
	if free == nil || free(got) {
		return got
	}
	if !config.Tiered(models) {
		return got
	}
	for _, m := range models {
		if m.Name != got.Name && m.Tier == got.Tier && available(avail, m) && free(m) {
			return m
		}
	}
	return got
}

// Next returns the model a task that FAILED on cur escalates to, skipping
// unavailable models. ok is false when there is nowhere to go: a same-tier
// retry would hit the same capability wall, and an untiered list has nothing
// past its last entry.
func Next(models []config.ModelConfig, cur string, avail func(config.ModelConfig) bool) (config.ModelConfig, bool) {
	i := indexOf(models, cur)
	if i < 0 {
		return config.ModelConfig{}, false
	}
	var cands []config.ModelConfig
	if config.Tiered(models) {
		// Only strictly stronger rungs qualify; slice order breaks ties
		// within a tier.
		for _, m := range models {
			if m.Tier > models[i].Tier {
				cands = append(cands, m)
			}
		}
		byTierAsc(cands)
	} else {
		cands = append([]config.ModelConfig{}, models[i+1:]...)
	}
	return firstAvailable(cands, avail)
}

// candidates orders every model other than want by how close a substitute it
// is: same-tier siblings first (same capability, different endpoint), then
// the nearest stronger tiers, then weaker tiers as a last resort; untiered
// lists have no ladder, so they just prefer the file order around want.
func candidates(models []config.ModelConfig, want config.ModelConfig) []config.ModelConfig {
	i := indexOf(models, want.Name)
	if i < 0 {
		// Unknown name has no tier or position to rank around: plain slice
		// order.
		return models
	}
	if !config.Tiered(models) {
		rest := make([]config.ModelConfig, 0, len(models)-1)
		rest = append(rest, models[i+1:]...)
		rest = append(rest, models[:i]...)
		return rest
	}
	var same, up, lower []config.ModelConfig
	for _, m := range models {
		switch {
		case m.Name == want.Name:
		case m.Tier == models[i].Tier:
			same = append(same, m)
		case m.Tier > models[i].Tier:
			up = append(up, m)
		default:
			lower = append(lower, m)
		}
	}
	byTierAsc(up)
	byTierDesc(lower)
	rest := make([]config.ModelConfig, 0, len(models)-1)
	rest = append(rest, same...)
	rest = append(rest, up...)
	rest = append(rest, lower...)
	return rest
}

// available treats a nil check as "everything is up" so callers can omit it.
func available(avail func(config.ModelConfig) bool, m config.ModelConfig) bool {
	return avail == nil || avail(m)
}

// firstAvailable picks the earliest candidate that passes the availability
// check.
func firstAvailable(cands []config.ModelConfig, avail func(config.ModelConfig) bool) (config.ModelConfig, bool) {
	for _, m := range cands {
		if available(avail, m) {
			return m, true
		}
	}
	return config.ModelConfig{}, false
}

// indexOf finds an entry by name, or -1 when absent.
func indexOf(models []config.ModelConfig, name string) int {
	for i, m := range models {
		if m.Name == name {
			return i
		}
	}
	return -1
}

// byTierAsc ranks the nearest (weakest) tier first; stable sort keeps slice
// order inside a tier.
func byTierAsc(cands []config.ModelConfig) {
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].Tier < cands[b].Tier })
}

// byTierDesc is the last-resort direction: the closest weaker tier comes
// first.
func byTierDesc(cands []config.ModelConfig) {
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].Tier > cands[b].Tier })
}
