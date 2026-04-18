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

func NewRoutingEmbeddingService(aiCfg config.AIConfig, health *model.HealthStore, selector *model.Selector) *RoutingEmbeddingService {
	clients := map[string]EmbeddingClient{
		"siliconflow": NewSiliconFlowClient(),     // 云端 API（SiliconFlow）
		"ollama":      NewOllamaEmbeddingClient(), // 本地模型（Ollama）
	}
	return &RoutingEmbeddingService{
		executor: model.NewRoutingExecutor[[][]float32](selector, health, aiCfg.Providers),
		aiCfg:    aiCfg,
		clients:  clients,
		selector: selector,
	}
}

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
