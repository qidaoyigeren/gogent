package rerank

import (
	"context"
	"gogent/internal/model"
)

// RerankResult 表示重排序后的文档结果
type RerankResult struct {
	Index int     // 原文档在输入列表中的索引
	Score float64 // 相关性分数（越高越相关）
	Text  string  // 文档内容
}

// RerankClient Rerank 提供商接口
// 核心职责：
// 1. 接收查询和文档列表
// 2. 调用 Rerank 模型计算相关性分数
// 3. 返回按分数排序的结果
type RerankClient interface {
	// Rerank 对文档列表进行重排序
	Rerank(ctx context.Context, target model.ModelTarget, query string, documents []string, topN int) ([]RerankResult, error)

	// Provider 返回提供商名称
	Provider() string
}

// RerankService 高层 Rerank 服务接口
// 屏蔽路由、降级等复杂逻辑
type RerankService interface {
	// Rerank 对文档进行重排序
	// 使用配置的默认 Rerank 模型
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]RerankResult, error)
}
