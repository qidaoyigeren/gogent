package model

import (
	"gogent/internal/config"
	"log/slog"
	"sort"
)

// Selector picks the best available model candidate.
type Selector struct {
	health *HealthStore
}

// NewSelector creates a model selector.
func NewSelector(health *HealthStore) *Selector {
	return &Selector{health: health}
}

// Select returns available candidates sorted by priority (ascending), filtered by circuit breaker.
func (s *Selector) Select(group config.ModelGroupConfig) []config.ModelCandidate {
	var available []config.ModelCandidate

	for _, c := range group.Candidates {
		if !c.IsEnabled() {
			continue
		}
		if !s.health.IsAvailable(c.ID) {
			slog.Debug("model filtered by circuit breaker", "model", c.ID)
			continue
		}
		available = append(available, c)
	}

	sort.Slice(available, func(i, j int) bool {
		return available[i].Priority < available[j].Priority
	})

	return available
}

// SelectChatCandidates returns candidates for chat, optionally requiring thinking capability.
// If deepThinking is true, only candidates with SupportsThinking=true are returned.
// The preferred default model becomes group.DeepThinkingModel (if set), otherwise group.DefaultModel.
func (s *Selector) SelectChatCandidates(group config.ModelGroupConfig, deepThinking bool) (preferredID string, candidates []config.ModelCandidate) {
	preferredID = group.DefaultModel
	if deepThinking && group.DeepThinkingModel != "" {
		preferredID = group.DeepThinkingModel
	}

	all := s.Select(group)
	if !deepThinking {
		return preferredID, all
	}

	var filtered []config.ModelCandidate
	for _, c := range all {
		if c.SupportsThinking {
			filtered = append(filtered, c)
		}
	}
	return preferredID, filtered
}

// SelectDefault returns the default model candidate. Falls back to first available.
func (s *Selector) SelectDefault(group config.ModelGroupConfig) (config.ModelCandidate, bool) {
	candidates := s.Select(group)
	if len(candidates) == 0 {
		return config.ModelCandidate{}, false
	}

	// Try to find the configured default
	for _, c := range candidates {
		if c.ID == group.DefaultModel {
			return c, true
		}
	}

	// Fallback to highest priority
	return candidates[0], true
}
