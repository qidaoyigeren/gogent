package ingestion

// Chunker：长文本 → 多段文本 + 每段 embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/embedding"
	"strings"
)

type ChunkingMode string

const (
	ChunkingModeFixedSize      ChunkingMode = "fixed_size"
	ChunkingModeStructureAware ChunkingMode = "structure_aware"
)

// ChunkerSettings：fixed_size 用 ChunkSize/OverlapSize；structure_aware 用 Target/Max/Min/OverlapChars 等。
type ChunkerSettings struct {
	Strategy       ChunkingMode `json:"strategy"`
	ChunkSize      int          `json:"chunkSize"`
	OverlapSize    int          `json:"overlapSize"`
	TargetChars    int          `json:"targetChars"`
	OverlapChars   int          `json:"overlapChars"`
	MaxChars       int          `json:"maxChars"`
	MinChars       int          `json:"minChars"`
	Separator      string       `json:"separator"`
	EmbeddingModel string       `json:"embeddingModel,omitempty"` // 非空且服务实现 ModelSelectable 时走指定模型
}

type ChunkerNode struct {
	embSvc         embedding.EmbeddingService
	defaultSize    int // parseSettings 未给 ChunkSize 时的默认窗口
	defaultOverlap int
	strategies     map[ChunkingMode]ChunkingStrategy
}

// NewChunkerNode 创建分块节点，并注册默认分块策略。
// chunkSize/overlap 是兜底配置；具体 pipeline 节点可通过 settings 覆盖。
func NewChunkerNode(embSvc embedding.EmbeddingService, chunkSize, overlap int) *ChunkerNode {
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if overlap < 0 {
		overlap = 128
	}
	return &ChunkerNode{
		embSvc:         embSvc,
		defaultSize:    chunkSize,
		defaultOverlap: overlap,
		strategies:     newDefaultChunkStrategyRegistry(),
	}
}

func (n *ChunkerNode) Name() string { return "chunker" }

// Execute 将解析后的文本切成 chunk，并为每个 chunk 生成 embedding。
// 输入优先使用 EnhancedText，这样 enhancer 节点产出的优化文本会覆盖 RawText；
// 输出写回 ingestCtx.Chunks，供 indexer 或 handler 统一持久化。
func (n *ChunkerNode) Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult {
	text := strings.TrimSpace(ingestCtx.EnhancedText)
	if text == "" {
		text = strings.TrimSpace(ingestCtx.RawText) // enhancer 未跑或为空则用 parser 输出
	}
	if text == "" {
		return NewNodeResultError(fmt.Errorf("可分块文本为空"))
	}
	settings := n.parseSettings(config.Settings)
	// 先切片再 embed：同一批 texts 保序，与后面 embeddings 下标一一对应
	texts := n.splitText(text, settings)
	if len(texts) == 0 {
		return NewNodeResult("未生成文本分块")
	}
	if n.embSvc == nil {
		return NewNodeResultError(fmt.Errorf("chunker requires embedding service"))
	}

	embeddings, err := n.embedTexts(ctx, settings.EmbeddingModel, texts)
	if err != nil {
		return NewNodeResultError(fmt.Errorf("embed chunks: %w", err))
	}
	if len(embeddings) != len(texts) {
		return NewNodeResultError(fmt.Errorf("embedding count mismatch: got %d, expected %d", len(embeddings), len(texts)))
	}

	ingestCtx.Chunks = make([]Chunk, len(texts))
	for i, text := range texts {
		metadata := map[string]string{"chunkIndex": fmt.Sprintf("%d", i)}
		for k, v := range ingestCtx.Metadata {
			metadata[k] = fmt.Sprintf("%v", v) // 文档级键全部 stringify，与 indexer/向量侧 map[string]string 对齐
		}
		ingestCtx.Chunks[i] = Chunk{
			// 无 DocID 时用 TaskID 前缀，避免多任务并发写 id 冲突（依赖上层保证唯一时更佳）
			ID:       fmt.Sprintf("%s_chunk_%d", fallbackID(ingestCtx.DocID, ingestCtx.TaskID), i),
			Content:  text,
			Vector:   embeddings[i],
			Metadata: metadata,
		}
	}

	return NewNodeResultWithOutput("分块完成", map[string]interface{}{
		"strategy":   settings.Strategy,
		"chunkCount": len(ingestCtx.Chunks),
		"chunkSize":  settings.ChunkSize,
		"overlap":    settings.OverlapSize,
	})
}

func (n *ChunkerNode) embedTexts(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	if selectable, ok := n.embSvc.(embedding.ModelSelectableEmbeddingService); ok && strings.TrimSpace(modelID) != "" {
		return selectable.EmbedWithModelID(ctx, modelID, texts)
	}
	// 未实现接口或 model 为空：走默认模型，多供应商共用一套 Embed
	return n.embSvc.Embed(ctx, texts)
}

// Process 是旧版 Document pipeline 的适配层，把旧结构转换成 IngestionContext，
// 再复用新的 Execute 逻辑，避免维护两套分块实现。
func (n *ChunkerNode) Process(ctx context.Context, doc *Document) error {
	metadata := map[string]interface{}{}
	for k, v := range doc.Metadata {
		metadata[k] = v
	}
	ingestCtx := &IngestionContext{
		DocID:    doc.ID,
		KBID:     doc.KBID,
		RawText:  doc.Parsed,
		Metadata: metadata,
	}
	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: string(IngestionNodeTypeChunker)})
	if !result.Success {
		return result.Error
	}
	doc.Chunks = ingestCtx.Chunks
	return nil
}

// parseSettings 合并 pipeline 节点配置和节点默认值。
// JSON 解析失败不会中断流程，而是使用默认配置；这样配置缺失时仍能跑通默认入库链。
func (n *ChunkerNode) parseSettings(raw json.RawMessage) ChunkerSettings {
	settings := ChunkerSettings{
		Strategy:     ChunkingModeFixedSize,
		ChunkSize:    n.defaultSize,
		OverlapSize:  n.defaultOverlap,
		TargetChars:  1400,
		OverlapChars: 0,
		MaxChars:     1800,
		MinChars:     600,
	}
	if len(raw) == 0 || string(raw) == "null" {
		return settings
	}
	_ = json.Unmarshal(raw, &settings) // 部分字段缺省时保留上面默认值
	if settings.Strategy == "" {
		settings.Strategy = ChunkingModeFixedSize
	}
	if settings.ChunkSize <= 0 {
		settings.ChunkSize = 512
	}
	if settings.OverlapSize < 0 {
		settings.OverlapSize = 128
	}
	if settings.TargetChars <= 0 {
		settings.TargetChars = 1400
	}
	if settings.OverlapChars < 0 {
		settings.OverlapChars = 0
	}
	if settings.MaxChars <= 0 {
		settings.MaxChars = 1800
	}
	if settings.MinChars <= 0 {
		settings.MinChars = 600
	}
	return settings
}

func (n *ChunkerNode) splitText(text string, settings ChunkerSettings) []string {
	strategy, ok := n.strategies[settings.Strategy]
	if !ok {
		strategy = n.strategies[ChunkingModeFixedSize] // 拼写错误或未来新策略未注册时的安全网
	}
	return strategy.Split(text, settings)
}

// fallbackID 生成 chunk ID 前缀：优先使用文档 ID，其次任务 ID，最后兜底常量。
func fallbackID(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "ingestion"
}
