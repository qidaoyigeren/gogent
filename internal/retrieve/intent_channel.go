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
type IntentDirectedChannel struct {
	embSvc    embedding.EmbeddingService // Embedding 服务（文本→向量）
	vectorSvc vector.VectorStoreService  // 向量存储服务（向量检索）
	minScore  float64                    // 最小意图分数门控（低于此分数跳过）
	topKMult  int                        // TopK 倍数（实际检索数 = TopK × topKMult）
}

func NewIntentDirectedChannel(embSvc embedding.EmbeddingService, vectorSvc vector.VectorStoreService, minScore float64, topKMult int) *IntentDirectedChannel {
	return &IntentDirectedChannel{
		embSvc:    embSvc,
		vectorSvc: vectorSvc,
		minScore:  minScore,
		topKMult:  topKMult,
	}
}

func (c *IntentDirectedChannel) Name() string { return "intent-directed" }

func (c *IntentDirectedChannel) Priority() int { return 1 }

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
func (c *IntentDirectedChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	//设置最小门控
	min := c.minScore
	if min <= 0 {
		min = 0.4
	}

	//过滤低于 minScore 的意图知识库
	kbIDs := reqCtx.IntentKBIDs
	if len(reqCtx.IntentKBMaxScore) > 0 {
		var filterd []string
		for _, kbID := range kbIDs {
			if reqCtx.IntentKBMaxScore[kbID] > min {
				filterd = append(filterd, kbID)
			}
		}
		kbIDs = filterd
	}
	if len(kbIDs) == 0 {
		return nil, nil
	}
	//将查询文本转换为向量
	queryVec, err := c.embSvc.EmbedSingle(ctx, reqCtx.Query)
	if err != nil {
		return nil, err
	}

	//实际检索数
	topK := reqCtx.TopK * c.topKMult
	//并行搜索每个意图知识库
	g, gCtx := errgroup.WithContext(ctx)
	var results []DocumentChunk
	var mu sync.Mutex
	for _, kbID := range kbIDs {
		g.Go(func() error {
			chunks, err := c.vectorSvc.Search(gCtx, kbID, queryVec, topK)
			if err != nil {
				slog.Warn("intent-directed search failed", "kb", kbID, "err", err)
				return nil
			}
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
			//合并所有结果
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
