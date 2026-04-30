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

// Rerank 实现 RerankClient 接口
// 返回文档原顺序，分数递减
//
// 工作流程：
// 1. 为每个文档创建 RerankResult
// 2. 分数按索引递减：1.0 - i*0.01
//   - 第 0 个文档：1.0
//   - 第 1 个文档：0.99
//   - 第 2 个文档：0.98
//   - ...
//
// 3. 按分数降序排序（保持原顺序）
// 4. 如果指定 topN，截断结果
// 5. 返回结果
//
// 为什么分数递减？
// - 模拟真实 Rerank 的排序效果
// - 保持文档的原始顺序（第一个最相关）
// - 避免分数相同导致的不确定性
func (c *NoopRerankClient) Rerank(_ context.Context, _ model.ModelTarget, _ string, documents []string, topN int) ([]RerankResult, error) {
	// 1. 为每个文档创建结果
	results := make([]RerankResult, len(documents))
	for i, doc := range documents {
		results[i] = RerankResult{
			Index: i,
			// 分数递减：第一个文档 1.0，第二个 0.99，依此类推
			Score: 1.0 - float64(i)*0.01,
			Text:  doc,
		}
	}

	// 2. 按分数降序排序（实际上已经是有序的）
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// 3. 截断 Top-N
	if topN > 0 && topN < len(results) {
		results = results[:topN]
	}

	return results, nil
}
