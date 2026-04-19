package rerank

import (
	"context"
	"fmt"
	"gogent/internal/config"
	"gogent/internal/model"
	"log/slog"
)

// RoutingRerankService 带路由功能的 Rerank 服务
// 核心职责：
// 1. 多模型候选：配置多个 Rerank 模型（如 bailian → noop）
// 2. 自动降级：当主模型失败时，自动尝试备用模型
// 3. 健康检查：记录模型成功/失败次数，实现熔断机制
type RoutingRerankService struct {
	executor *model.RoutingExecutor[[]RerankResult] // 路由执行器
	aiCfg    config.AIConfig                        // AI 配置
	clients  map[string]RerankClient                // 提供商 -> 客户端映射
}

func NewRoutingRerankService(aiCfg config.AIConfig, health *model.HealthStore, selector *model.Selector) *RoutingRerankService {
	// 初始化提供商客户端映射
	clients := map[string]RerankClient{
		"bailian": NewBaiLianRerankClient(), // 阿里云百炼 Rerank
		"noop":    NewNoopRerankClient(),    // 空实现（降级用）
	}
	return &RoutingRerankService{
		// 创建路由执行器
		// 泛型参数 []RerankResult 表示返回值类型
		executor: model.NewRoutingExecutor[[]RerankResult](selector, health, aiCfg.Providers),
		aiCfg:    aiCfg,
		clients:  clients,
	}
}

// Rerank 对文档列表进行重排序（使用默认路由）
// 自动选择健康且可用的模型
func (s *RoutingRerankService) Rerank(ctx context.Context, query string, documents []string, topN int) ([]RerankResult, error) {
	return s.executor.Execute(ctx, s.aiCfg.Rerank, "rerank", func(ctx context.Context, target model.ModelTarget) ([]RerankResult, error) {
		// 根据提供商名称查找对应的客户端
		client, ok := s.clients[target.Provider]
		if !ok {
			return nil, fmt.Errorf("no rerank client for provider %s", target.Provider)
		}
		// 记录调试日志
		slog.Debug("reranking via", "provider", target.Provider, "docs", len(documents))
		// 调用具体客户端的实现
		return client.Rerank(ctx, target, query, documents, topN)
	})
}
