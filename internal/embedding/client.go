package embedding

import (
	"context"
	"gogent/internal/model"
)

// EmbeddingClient 是 embedding 提供商的接口定义
// 核心职责：
// 1. 定义统一的 embedding 调用接口（支持多文本批量）
// 2. 屏蔽不同提供商（SiliconFlow、Ollama）的 API 差异
// 3. 通过 Provider() 方法标识客户端类型，用于路由分发
type EmbeddingClient interface {
	// Embed 返回给定文本的向量表示
	Embed(ctx context.Context, target model.ModelTarget, texts []string) ([][]float32, error)

	// Provider 返回此客户端处理的提供商名称
	// 用于路由层根据提供商名称选择对应的客户端
	// 例如："siliconflow"、"ollama"
	Provider() string
}

// EmbeddingService 是高层 embedding 服务接口
// 与 EmbeddingClient 的区别：
// - EmbeddingClient: 面向单个提供商的实现
// - EmbeddingService: 面向调用方，屏蔽路由、降级等复杂逻辑
type EmbeddingService interface {
	// Embed 批量获取多个文本的向量
	// 使用配置中的默认 embedding 模型
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// EmbedSingle 获取单个文本的向量
	// 内部调用 Embed，然后取第一个结果
	EmbedSingle(ctx context.Context, text string) ([]float32, error)
}

// ModelSelectableEmbeddingService 支持模型选择的 embedding 服务接口
// 继承 EmbeddingService，额外支持强制指定模型 ID
type ModelSelectableEmbeddingService interface {
	EmbeddingService

	// EmbedWithModelID 使用指定模型 ID 获取向量
	// 用于需要精确控制使用哪个 embedding 模型的场景
	// 参数：
	//   - modelID: 模型配置 ID（对应 YAML 中的 candidate.id）
	EmbedWithModelID(ctx context.Context, modelID string, texts []string) ([][]float32, error)

	// EmbedSingleWithModelID 使用指定模型 ID 获取单个文本的向量
	EmbedSingleWithModelID(ctx context.Context, modelID string, text string) ([]float32, error)
}
