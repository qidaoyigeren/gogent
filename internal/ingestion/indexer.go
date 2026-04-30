package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/vector"
	"log/slog"
	"strings"
)

/*
职责：Chunks → 向量库持久化
关键逻辑：
  1. 确定 Collection 名称
     - 优先级：VectorSpaceID > defaultCollection
  2. 确定向量维度
     - 优先级：配置值 > 第一个chunk的向量长度
  3. EnsureCollection(collection, dim)
  4. Chunk 转 ChunkData，写入向量库

SkipIndexerWrite 机制：
  - Handler可设置此标志跳过内部写入
  - 由Handler统一处理DB+向量，避免双写竞态

输出：
  - 向量库持久化完成
*/

type IndexerSettings struct {
	EmbeddingModel string   `json:"embeddingModel"`
	MetadataFields []string `json:"metadataFields"`
}

type IndexerNode struct {
	vectorSvc         vector.VectorStoreService
	defaultDimension  int
	defaultCollection string
}

// NewIndexerNode 创建向量索引节点。
// defaultCollection 是未显式指定知识库 collection 时的兜底向量集合名。
func NewIndexerNode(vectorSvc vector.VectorStoreService, dimension int, defaultCollection string) *IndexerNode {
	if defaultCollection == "" {
		defaultCollection = "rag_default_store"
	}
	return &IndexerNode{
		vectorSvc:         vectorSvc,
		defaultDimension:  dimension,
		defaultCollection: defaultCollection,
	}
}

func (n *IndexerNode) Name() string { return "indexer" }

// Execute 将 chunker 生成的 Chunks 写入向量库。
// 它会先确定 collection 和向量维度，再把 ingestion.Chunk 转成 vector.ChunkData。
// 对文档 Handler 来说，这一步可以被 SkipIndexerWrite 跳过，由 Handler 自己统一写
// 数据库 chunk 行和向量库，避免双写顺序失控。
func (n *IndexerNode) Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult {
	if len(ingestCtx.Chunks) == 0 {
		return NewNodeResultError(fmt.Errorf("没有可索引的分块"))
	}
	if n.vectorSvc == nil {
		return NewNodeResultError(fmt.Errorf("indexer requires vector store service"))
	}

	settings := n.parseSettings(config.Settings)
	collection := n.resolveCollectionName(ingestCtx)
	dim := n.resolveDimension(ingestCtx)
	if dim <= 0 {
		return NewNodeResultError(fmt.Errorf("cannot infer vector dimension from chunks"))
	}
	if err := n.vectorSvc.EnsureCollection(ctx, collection, dim); err != nil {
		// 仅告警：有的实现 Ensure 幂等性差但 Index 仍成功；真正失败会在下面 IndexChunks 暴露
		slog.Warn("ensure collection failed", "collection", collection, "err", err)
	}

	chunkData := make([]vector.ChunkData, len(ingestCtx.Chunks))
	for i, chunk := range ingestCtx.Chunks {
		// 维度校验能尽早发现 embedding 模型和向量集合配置不一致的问题。
		if len(chunk.Vector) != dim {
			return NewNodeResultError(fmt.Errorf("chunk %d dimension %d != expected %d", i, len(chunk.Vector), dim))
		}
		chunkData[i] = vector.ChunkData{
			ID:       chunk.ID,
			DocID:    fallbackID(ingestCtx.DocID, ingestCtx.TaskID),
			Index:    i,
			Content:  chunk.Content,
			Vector:   chunk.Vector,
			Metadata: n.resolveChunkMetadata(ingestCtx, chunk, settings.MetadataFields),
		}
	}
	if ingestCtx.SkipIndexerWrite {
		// doc handler 路径：避免 indexer 与 handler 双写向量/双写 chunk 行
		return NewNodeResultWithOutput("已准备分块，向量写入由调用方统一完成", map[string]interface{}{
			"collectionName": collection,
			"chunkCount":     len(chunkData),
			"dimension":      dim,
		})
	}
	if err := n.vectorSvc.IndexChunks(ctx, collection, chunkData); err != nil {
		return NewNodeResultError(fmt.Errorf("index chunks: %w", err))
	}
	return NewNodeResultWithOutput("索引完成", map[string]interface{}{
		"collectionName": collection,
		"chunkCount":     len(chunkData),
		"dimension":      dim,
	})
}

// Process 是旧版 Document pipeline 的索引适配入口。
func (n *IndexerNode) Process(ctx context.Context, doc *Document) error {
	metadata := map[string]interface{}{}
	for k, v := range doc.Metadata {
		metadata[k] = v
	}
	ingestCtx := &IngestionContext{
		DocID:    doc.ID,
		KBID:     doc.KBID,
		Metadata: metadata,
		Chunks:   doc.Chunks,
	}
	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: string(IngestionNodeTypeIndexer)})
	if !result.Success {
		return result.Error
	}
	return nil
}

// parseSettings 读取 indexer 节点配置，目前主要用于控制 metadata 字段白名单。
func (n *IndexerNode) parseSettings(raw json.RawMessage) IndexerSettings {
	var settings IndexerSettings
	if len(raw) == 0 || string(raw) == "null" {
		return settings
	}
	_ = json.Unmarshal(raw, &settings)
	return settings
}

// resolveCollectionName 决定写入哪个向量集合。
// 知识库级 VectorSpaceID 优先级最高，否则使用服务启动时配置的默认集合。
func (n *IndexerNode) resolveCollectionName(ctx *IngestionContext) string {
	if ctx.VectorSpaceID != nil && strings.TrimSpace(ctx.VectorSpaceID.LogicalName) != "" {
		return ctx.VectorSpaceID.LogicalName
	}
	return n.defaultCollection
}

// resolveDimension：启动时注入的 defaultDimension 优先；为 0 时退化为「首块向量长度」。
func (n *IndexerNode) resolveDimension(ctx *IngestionContext) int {
	if n.defaultDimension > 0 {
		return n.defaultDimension
	}
	if len(ctx.Chunks) == 0 {
		return 0
	}
	return len(ctx.Chunks[0].Vector)
}

// resolveChunkMetadata 合并文档级 metadata 与 chunk 级 metadata。
// fields 为空表示全部透传；fields 非空时只挑选指定字段，最后保留 chunk 自身的字段
// 作为补充，确保 chunkIndex 等基础信息不丢失。
func (n *IndexerNode) resolveChunkMetadata(ctx *IngestionContext, chunk Chunk, fields []string) map[string]string {
	metadata := map[string]string{}
	if len(fields) == 0 {
		// 白名单为空：文档级 metadata 全量进向量侧（注意体积；大对象应在上游控制）
		for k, v := range ctx.Metadata {
			metadata[k] = fmt.Sprintf("%v", v)
		}
	} else {
		// 白名单非空：只带列出的键；chunk 级可补同一 key（块内更细粒度）
		for _, field := range fields {
			if value, ok := ctx.Metadata[field]; ok {
				metadata[field] = fmt.Sprintf("%v", value)
			}
			if chunk.Metadata != nil {
				if value, ok := chunk.Metadata[field]; ok {
					metadata[field] = value
				}
			}
		}
	}
	for k, v := range chunk.Metadata {
		if _, ok := metadata[k]; !ok {
			metadata[k] = v // 补 chunk 独有的键（如 chunkIndex），避免被白名单裁掉后丢失
		}
	}
	return metadata
}
