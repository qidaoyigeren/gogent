package model

import (
	"context"
	"fmt"
	"gogent/internal/config"
	"log/slog"
)

// CallerFunc is a function that calls a model and returns a result.
type CallerFunc[T any] func(ctx context.Context, target ModelTarget) (T, error)

// RoutingExecutor executes a call with automatic failover across candidates.
type RoutingExecutor[T any] struct {
	selector  *Selector
	health    *HealthStore
	providers map[string]config.ProviderConfig
}

// NewRoutingExecutor creates a new routing executor.
func NewRoutingExecutor[T any](
	selector *Selector,
	health *HealthStore,
	providers map[string]config.ProviderConfig,
) *RoutingExecutor[T] {
	return &RoutingExecutor[T]{
		selector:  selector,
		health:    health,
		providers: providers,
	}
}

// Execute tries each candidate in priority order until one succeeds.
func (r *RoutingExecutor[T]) Execute(
	ctx context.Context,
	group config.ModelGroupConfig,
	endpointKey string,
	caller CallerFunc[T],
) (T, error) {
	candidates := r.selector.Select(group)
	return r.ExecuteWithCandidates(ctx, group, endpointKey, candidates, caller)

}

// ExecuteWithCandidates tries each provided candidate in order until one succeeds.
// This allows callers (e.g. chat thinking mode) to filter/reorder candidates before execution.
func (r *RoutingExecutor[T]) ExecuteWithCandidates(
	ctx context.Context,
	group config.ModelGroupConfig,
	endpointKey string,
	candidates []config.ModelCandidate,
	caller CallerFunc[T],
) (T, error) {
	if len(candidates) == 0 {
		var zero T
		return zero, fmt.Errorf("no available model candidates for group (default=%s)", group.DefaultModel)
	}
	var lastErr error
	for _, c := range candidates {
		provider, ok := r.providers[c.Provider]
		if !ok {
			slog.Warn("provider not found", "provider", c.Provider, "model", c.ID)
			continue
		}

		target := ResolveTarget(c, provider, endpointKey)
		slog.Debug("trying model", "id", c.ID, "url", target.URL)

		result, err := caller(ctx, target)
		if err == nil {
			lastErr = err
			r.health.RecordFailure(c.ID)
			slog.Warn("model call failed, trying next", "model", c.ID, "err", err)
			continue
		}
		r.health.RecordSuccess(c.ID)
		return result, nil
	}

	var zero T
	return zero, fmt.Errorf("all model candidates exhausted: %w", lastErr)
}
