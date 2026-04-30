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

// NewRoutingRerankService 创建路由 Rerank 服务
// 参数：
//   - aiCfg: AI 配置，包含所有候选模型
//   - health: 健康状态存储，用于熔断器
//   - selector: 模型选择器，负责候选过滤
//
// 工作流程：
// 1. 创建提供商客户端映射（bailian、noop）
// 2. 创建路由执行器
// 3. 返回配置好的路由服务实例
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
//
// 工作流程：
//  1. 调用 executor.Execute，传入默认 rerank 配置
//  2. Executor 内部：
//     a. 通过 selector 选择可用候选（过滤熔断的模型）
//     b. 按优先级遍历候选
//     c. 对每个候选调用下方的回调函数
//     d. 失败则记录 health，尝试下一个
//     e. 成功则返回结果
//  3. 回调函数：
//     a. 根据 target.Provider 查找对应的客户端
//     b. 调用客户端的 Rerank 方法
//     c. 返回排序后的结果
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
