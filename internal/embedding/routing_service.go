package embedding

import (
	"context"
	"fmt"
	"gogent/internal/config"
	"gogent/internal/model"
	"log/slog"
)

// RoutingEmbeddingService 是带路由功能的 embedding 服务
// 核心职责：
// 1. 多模型候选：配置多个 embedding 模型（如 siliconflow → ollama）
// 2. 自动降级：当主模型失败时，自动尝试备用模型
// 3. 健康检查：记录模型成功/失败次数，实现熔断机制
// 4. 模型选择：支持通过 modelID 强制指定模型
type RoutingEmbeddingService struct {
	executor *model.RoutingExecutor[[][]float32] // 路由执行器，负责降级逻辑
	aiCfg    config.AIConfig                     // AI 配置（包含候选模型列表）
	clients  map[string]EmbeddingClient          // 提供商 -> 客户端映射
	selector *model.Selector                     // 模型选择器，负责过滤和排序
}

// NewRoutingEmbeddingService 创建路由 embedding 服务
// 参数：
//   - aiCfg: AI 配置，包含所有候选模型
//   - health: 健康状态存储，用于熔断器
//   - selector: 模型选择器，负责候选过滤
//
// 工作流程：
// 1. 创建提供商客户端映射（siliconflow、ollama）
// 2. 创建路由执行器，传入 embedding 类型的泛型
// 3. 返回配置好的路由服务实例
func NewRoutingEmbeddingService(aiCfg config.AIConfig, health *model.HealthStore, selector *model.Selector) *RoutingEmbeddingService {
	// 初始化提供商客户端映射
	// key: 提供商名称，value: 对应的客户端实现
	clients := map[string]EmbeddingClient{
		"siliconflow": NewSiliconFlowClient(),     // 云端 API（SiliconFlow）
		"ollama":      NewOllamaEmbeddingClient(), // 本地模型（Ollama）
	}
	return &RoutingEmbeddingService{
		// 创建路由执行器
		// 泛型参数 [][]float32 表示返回值类型
		// 与 chat 的 RoutingExecutor[<-chan StreamDelta] 不同
		executor: model.NewRoutingExecutor[[][]float32](selector, health, aiCfg.Providers),
		aiCfg:    aiCfg,
		clients:  clients,
		selector: selector,
	}
}

// Embed 批量获取文本向量（使用默认路由）
// 这是最常用的接口，自动选择健康且可用的模型
//
// 工作流程：
//  1. 调用 executor.Execute，传入默认 embedding 配置
//  2. Executor 内部：
//     a. 通过 selector 选择可用候选（过滤熔断的模型）
//     b. 按优先级遍历候选
//     c. 对每个候选调用下方的回调函数
//     d. 失败则记录 health，尝试下一个
//     e. 成功则返回结果
//  3. 回调函数：
//     a. 根据 target.Provider 查找对应的客户端
//     b. 调用客户端的 Embed 方法
//     c. 返回向量结果
func (s *RoutingEmbeddingService) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return s.executor.Execute(ctx, s.aiCfg.Embedding, "embedding", func(ctx context.Context, target model.ModelTarget) ([][]float32, error) {
		client, ok := s.clients[target.Provider]
		if !ok {
			return nil, fmt.Errorf("no embedding client for provider %s", target.Provider)
		}
		slog.Debug("embedding via", "provider", target.Provider, "model", target.Model, "count", len(texts))
		return client.Embed(ctx, target, texts)
	})
}

// EmbedWithModelID 使用指定模型 ID 获取文本向量
// 用于需要精确控制使用哪个 embedding 模型的场景
//
// 与 Embed 的区别：
// - Embed: 使用 selector 自动选择模型
// - EmbedWithModelID: 强制使用指定的模型（通过 modelID 过滤）
//
// 工作流程：
// 1. 调用 selectEmbeddingCandidates 过滤候选（只保留 modelID 匹配的）
// 2. 调用 executor.ExecuteWithCandidates，传入过滤后的候选列表
// 3. Executor 内部遍历候选（此时只有一个），调用回调函数
// 4. 回调函数与 Embed 相同
func (s *RoutingEmbeddingService) EmbedWithModelID(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	candidates := s.selectEmbeddingCandidates(modelID)
	return s.executor.ExecuteWithCandidates(ctx, s.aiCfg.Embedding, "embedding", candidates, func(ctx context.Context, target model.ModelTarget) ([][]float32, error) {
		client, ok := s.clients[target.Provider]
		if !ok {
			return nil, fmt.Errorf("no embedding client for provider %s", target.Provider)
		}
		// 记录更详细的日志（包含 modelID）
		slog.Debug("embedding via selected model", "provider", target.Provider, "model", target.Model, "modelID", modelID, "count", len(texts))
		return client.Embed(ctx, target, texts)
	})
}

// EmbedSingle 获取单个文本的向量
// 内部调用 Embed，然后取第一个结果
//
// 使用场景：
// - 查询重写：将用户问题转换为向量
// - 意图识别：将问题与意图树匹配
func (s *RoutingEmbeddingService) EmbedSingle(ctx context.Context, text string) ([]float32, error) {
	results, err := s.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("empty embedding result")
	}
	return results[0], nil
}

// EmbedSingleWithModelID 使用指定模型 ID 获取单个文本的向量
// 内部调用 EmbedWithModelID，然后取第一个结果
func (s *RoutingEmbeddingService) EmbedSingleWithModelID(ctx context.Context, modelID string, text string) ([]float32, error) {
	results, err := s.EmbedWithModelID(ctx, modelID, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("empty embedding result")
	}
	return results[0], nil
}

// selectEmbeddingCandidates 选择 embedding 候选模型
// 参数：
//   - modelID: 模型 ID（为空则返回所有候选）
//
// 工作流程：
// 1. 调用 selector.Select 获取所有可用候选（过滤熔断的）
// 2. 如果 modelID 为空，返回所有候选
// 3. 如果 modelID 不为空，过滤出匹配的候选
func (s *RoutingEmbeddingService) selectEmbeddingCandidates(modelID string) []config.ModelCandidate {
	candidates := s.selector.Select(s.aiCfg.Embedding)
	if modelID == "" {
		return candidates
	}
	filtered := make([]config.ModelCandidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.ID == modelID {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}
