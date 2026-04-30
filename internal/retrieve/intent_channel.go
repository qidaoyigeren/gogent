package retrieve

import (
	"context"
	"gogent/internal/embedding"
	"gogent/internal/vector"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"
)

// IntentDirectedChannel 意图定向检索通道
// 核心职责：
// 1. 根据意图识别结果，检索特定的知识库
// 2. 只搜索意图匹配的知识库（IntentKBIDs）
// 3. 通过 minScore 门控过滤低置信度的意图
//
// 工作流程：
// 1. 检查是否有意图匹配的知识库
// 2. 过滤低于 minScore 的意图
// 3. 将查询文本转换为向量
// 4. 并行搜索每个意图知识库
// 5. 合并所有知识库的检索结果
//
// 启用条件：
// - reqCtx.IntentKBIDs 非空（有意图匹配的知识库）
type IntentDirectedChannel struct {
	embSvc    embedding.EmbeddingService // Embedding 服务（文本→向量）
	vectorSvc vector.VectorStoreService  // 向量存储服务（向量检索）
	minScore  float64                    // 最小意图分数门控（低于此分数跳过）
	topKMult  int                        // TopK 倍数（实际检索数 = TopK × topKMult）
}

// NewIntentDirectedChannel 创建意图定向通道
// 参数：
//   - embSvc: Embedding 服务
//   - vectorSvc: 向量存储服务
//   - minScore: 最小意图分数（建议 0.4）
//   - topKMult: TopK 倍数（建议 2-3）
func NewIntentDirectedChannel(embSvc embedding.EmbeddingService, vectorSvc vector.VectorStoreService, minScore float64, topKMult int) *IntentDirectedChannel {
	return &IntentDirectedChannel{
		embSvc:    embSvc,
		vectorSvc: vectorSvc,
		minScore:  minScore,
		topKMult:  topKMult,
	}
}

// Name 返回通道名称
func (c *IntentDirectedChannel) Name() string { return "intent-directed" }

// Priority 返回通道优先级（1 = 最高优先级）
// 意图定向通道优先于全局向量通道
func (c *IntentDirectedChannel) Priority() int { return 1 }

// IsEnabled 判断通道是否启用
// 启用条件：有意图匹配的知识库
func (c *IntentDirectedChannel) IsEnabled(_ context.Context, reqCtx *RetrievalContext) bool {
	return len(reqCtx.IntentKBIDs) > 0
}

// Search 执行意图定向检索
//
// 工作流程：
// 1. 过滤低于 minScore 的意图知识库
// 2. 将查询文本转换为向量
// 3. 并行搜索每个意图知识库
// 4. 合并所有结果
//
// 容错策略：
// - 单个知识库搜索失败：非致命，记录警告，继续其他知识库
// - Embedding 失败：致命，返回错误
func (c *IntentDirectedChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	// 1. 设置最小分数门控
	min := c.minScore
	if min <= 0 {
		min = 0.4 // 默认 0.4
	}

	// 2. 过滤低置信度的意图知识库
	kbIDs := reqCtx.IntentKBIDs
	if len(reqCtx.IntentKBMaxScore) > 0 {
		var filtered []string
		for _, kbID := range reqCtx.IntentKBIDs {
			// 只保留分数 >= minScore 的知识库
			if sc, ok := reqCtx.IntentKBMaxScore[kbID]; ok && sc >= min {
				filtered = append(filtered, kbID)
			}
		}
		kbIDs = filtered
	}

	// 如果没有符合条件的知识库，返回空
	if len(kbIDs) == 0 {
		return nil, nil
	}

	// 3. 将查询文本转换为向量
	queryVec, err := c.embSvc.EmbedSingle(ctx, reqCtx.Query)
	if err != nil {
		return nil, err
	}

	// 4. 计算实际检索数量
	topK := reqCtx.TopK * c.topKMult
	if topK <= 0 {
		topK = 10 // 默认 10
	}

	// 5. 并行搜索每个意图知识库
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	var results []DocumentChunk

	for _, kbID := range kbIDs {
		kbID := kbID // 捕获循环变量
		g.Go(func() error {
			// 搜索单个知识库
			chunks, err := c.vectorSvc.Search(gCtx, kbID, queryVec, topK)
			if err != nil {
				// ⭐ 非致命错误：记录警告，返回 nil
				slog.Warn("intent-directed search failed", "kb", kbID, "err", err)
				return nil
			}

			// 转换结果格式
			var converted []DocumentChunk
			for _, sr := range chunks {
				converted = append(converted, DocumentChunk{
					ID:          sr.ID,
					Content:     sr.Content,
					Score:       sr.Score,
					KBID:        kbID,
					ChannelName: c.Name(),
					Metadata:    sr.Metadata,
				})
			}

			// 合并结果（需要加锁）
			mu.Lock()
			results = append(results, converted...)
			mu.Unlock()
			return nil
		})
	}

	// 6. 等待所有搜索完成
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return results, nil
}
