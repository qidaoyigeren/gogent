package retrieve

import (
	"context"
	"gogent/internal/embedding"
	"gogent/internal/vector"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

// VectorGlobalChannel 全局向量检索通道（兜底通道）
// 核心职责：
// 1. 当意图通道未启用或置信度低时，作为兜底检索
// 2. 搜索所有知识库（全局检索）
// 3. 通过置信度门控控制是否启用
type VectorGlobalChannel struct {
	embSvc              embedding.EmbeddingService // Embedding 服务
	vectorSvc           vector.VectorStoreService  // 向量存储服务
	defaultCollection   string                     // 默认集合名称
	confidenceThreshold float64                    // 置信度阈值（低于此阈值启用全局检索）
	topKMult            int                        // TopK 倍数
	db                  *gorm.DB                   // 数据库连接（用于查询所有知识库列表）
}

func NewVectorGlobalChannel(
	embSvc embedding.EmbeddingService,
	vectorSvc vector.VectorStoreService,
	defaultCollection string,
	confidenceThreshold float64,
	topKMult int,
	db *gorm.DB,
) *VectorGlobalChannel {
	return &VectorGlobalChannel{
		embSvc:              embSvc,
		vectorSvc:           vectorSvc,
		defaultCollection:   defaultCollection,
		confidenceThreshold: confidenceThreshold,
		topKMult:            topKMult,
		db:                  db,
	}
}

func (c *VectorGlobalChannel) Name() string { return "vector-global" }

func (c *VectorGlobalChannel) Priority() int { return 10 }

func (c *VectorGlobalChannel) IsEnabled(_ context.Context, reqCtx *RetrievalContext) bool {
	if reqCtx == nil {
		return false
	}
	// 无意图分数 → 启用全局检索
	if reqCtx.IntentScoreCount == 0 {
		return true
	}
	// 意图分数低于阈值 → 启用全局检索
	return reqCtx.MaxIntentScore < c.confidenceThreshold
}

func (c *VectorGlobalChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	queryVec, err := c.embSvc.EmbedSingle(ctx, reqCtx.Query)
	if err != nil {
		return nil, err
	}

	topK := c.topKMult * reqCtx.TopK
	if topK <= 0 {
		topK = 15
	}

	collections := []string{c.defaultCollection}
	if c.db != nil {
		if names, err := distinctKnowledgeCollectionNames(ctx, c.db); err != nil {
			// ⭐ 非致命错误：记录警告，使用默认集合
			slog.Warn("vector-global list collections failed", "err", err)
		} else if len(names) > 0 {
			collections = names
		}
	}
	g, gCtx := errgroup.WithContext(ctx)
	var results []DocumentChunk
	var mu sync.Mutex
	for _, collection := range collections {
		g.Go(func() error {
			chunks, err := c.vectorSvc.Search(gCtx, collection, queryVec, topK)
			if err != nil {
				// ⭐ 非致命错误：记录警告，返回 nil
				slog.Warn("vector-global search failed", "collection", collection, "err", err)
				return nil
			}
			var converted []DocumentChunk
			for _, chunk := range chunks {
				converted = append(converted, DocumentChunk{
					ID:          chunk.ID,
					Content:     chunk.Content,
					Score:       chunk.Score,
					KBID:        collection,
					ChannelName: c.Name(),
					Metadata:    chunk.Metadata,
				})
			}
			mu.Lock()
			results = append(results, converted...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func distinctKnowledgeCollectionNames(ctx context.Context, db *gorm.DB) ([]string, error) {
	var names []string
	err := db.WithContext(ctx).
		Where("deleted = 0").
		Where("collection_name IS NOT NULL AND collection_name != ?", "").
		Distinct("collection_name").
		Order("collection_name ASC").
		Pluck("collection_name", &names).Error
	if err != nil {
		return nil, err
	}
	return names, nil
}
