package retrieve

import (
	"context"
	"gogent/internal/embedding"
	"gogent/internal/entity"
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

// NewVectorGlobalChannel 创建全局向量通道
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

// Name 返回通道名称
func (c *VectorGlobalChannel) Name() string { return "vector-global" }

// Priority 返回通道优先级（10 = 低于意图通道）
func (c *VectorGlobalChannel) Priority() int { return 10 }

// IsEnabled 判断通道是否启用
// 启用逻辑：
// 1. 如果无意图分类器分数 → 启用（说明意图识别未匹配）
// 2. 如果最大意图分数低于阈值 → 启用（说明意图置信度低）
// 3. 否则 → 不启用（意图通道会处理）
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

// Search 执行全局向量检索
//
// 工作流程：
// 1. 将查询文本转换为向量
// 2. 获取所有知识库列表（从数据库或默认集合）
// 3. 并行搜索每个知识库
// 4. 合并所有结果
//
// 容错策略：
// - 单个知识库搜索失败：非致命，记录警告，继续其他知识库
// - 获取知识库列表失败：非致命，使用默认集合
// - Embedding 失败：致命，返回错误
func (c *VectorGlobalChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	// 1. 将查询文本转换为向量
	queryVec, err := c.embSvc.EmbedSingle(ctx, reqCtx.Query)
	if err != nil {
		return nil, err
	}

	// 2. 计算实际检索数量
	topK := reqCtx.TopK * c.topKMult
	if topK <= 0 {
		topK = 15 // 默认 15（比意图通道多一些）
	}

	// 3. 获取知识库列表
	collections := []string{c.defaultCollection}
	if c.db != nil {
		// 从数据库查询所有知识库的 collection_name
		if names, err := distinctKnowledgeCollectionNames(ctx, c.db); err != nil {
			// ⭐ 非致命错误：记录警告，使用默认集合
			slog.Warn("vector-global list collections failed", "err", err)
		} else if len(names) > 0 {
			collections = names
		}
	}

	// 4. 并行搜索每个知识库
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	var merged []DocumentChunk

	for _, coll := range collections {
		coll := coll // 捕获循环变量
		g.Go(func() error {
			// 搜索单个知识库
			chunks, err := c.vectorSvc.Search(gCtx, coll, queryVec, topK)
			if err != nil {
				// ⭐ 非致命错误：记录警告，返回 nil
				slog.Warn("vector-global search failed", "collection", coll, "err", err)
				return nil
			}

			// 转换结果格式
			var part []DocumentChunk
			for _, sr := range chunks {
				part = append(part, DocumentChunk{
					ID:          sr.ID,
					Content:     sr.Content,
					Score:       sr.Score,
					KBID:        coll,
					ChannelName: c.Name(),
					Metadata:    sr.Metadata,
				})
			}

			// 合并结果（需要加锁）
			mu.Lock()
			merged = append(merged, part...)
			mu.Unlock()
			return nil
		})
	}

	// 5. 等待所有搜索完成
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return merged, nil
}

// distinctKnowledgeCollectionNames 查询所有知识库的集合名称
// 用于动态发现所有需要检索的知识库
func distinctKnowledgeCollectionNames(ctx context.Context, db *gorm.DB) ([]string, error) {
	var names []string
	err := db.WithContext(ctx).Model(&entity.KnowledgeBaseDO{}).
		Where("deleted = 0").                                              // 排除已删除
		Where("collection_name IS NOT NULL AND collection_name != ?", ""). // 排除空值
		Distinct("collection_name").                                       // 去重
		Order("collection_name ASC").                                      // 排序
		Pluck("collection_name", &names).Error
	if err != nil {
		return nil, err
	}
	return names, nil
}
