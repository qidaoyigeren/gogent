package rerank

import (
	"context"
	"gogent/internal/model"
	"sort"
)

type NoopRerankClient struct{}

func NewNoopRerankClient() *NoopRerankClient { return &NoopRerankClient{} }

// Provider 返回提供商名称
func (c *NoopRerankClient) Provider() string { return "noop" }

func (c *NoopRerankClient) Rerank(_ context.Context, _ model.ModelTarget, _ string, documents []string, topN int) ([]RerankResult, error) {
	results := make([]RerankResult, len(documents))
	for i, doc := range documents {
		results[i] = RerankResult{
			Index: i,
			Score: 1.0 - float64(i)*0.01,
			Text:  doc,
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if topN > 0 && topN < len(results) {
		results = results[:topN]
	}
	return results, nil
}
