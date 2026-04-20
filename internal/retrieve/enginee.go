package retrieve

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"
)

// MultiChannelEngine 多通道检索引擎
// 核心职责：
// 1. 并行执行多个检索通道（intent-directed、vector-global）
// 2. 聚合各通道的检索结果
// 3. 应用后处理器（去重、重排序）
// 4. 最终截断到 TopK
type MultiChannelEngine struct {
	channels       []SearchChannel // 检索通道列表
	postProcessors []PostProcessor // 后处理器列表（按 Order 排序）
}

func NewMultiChannelEngine(channels []SearchChannel, processors []PostProcessor) *MultiChannelEngine {
	sorted := make([]PostProcessor, len(processors))
	copy(sorted, processors)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Order() < sorted[j].Order()
	})
	return &MultiChannelEngine{
		channels:       channels,
		postProcessors: sorted,
	}
}

// Retrieve 执行多通道并行检索
// 容错策略：
// - 通道失败：非致命，记录警告日志，继续其他通道
// - 后处理器失败：非致命，记录警告日志，使用上一步结果
// - 无启用通道：返回 nil，不报错
func (e *MultiChannelEngine) Retrieve(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	// 1. 过滤启用的通道
	var enabled []SearchChannel
	for _, c := range e.channels {
		if c.IsEnabled(ctx, reqCtx) {
			enabled = append(enabled, c)
		}
	}
	// 2. 按优先级排序
	sort.Slice(enabled, func(i, j int) bool {
		return enabled[i].Priority() < enabled[j].Priority()
	})
	// 3. 检查是否有启用的通道
	if len(enabled) == 0 {
		slog.Warn("no enabled search channels")
		return nil, nil
	}
	// 4. 并行执行通道搜索
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	var allChunks []DocumentChunk
	for _, ch := range enabled {
		ch := ch
		g.Go(func() error {
			slog.Debug("executing search channel", "channel", ch.Name(), "priority", ch.Priority())

			chunks, err := ch.Search(gCtx, reqCtx)
			if err != nil {
				slog.Warn("channel search failed", "channel", ch.Name(), "err", err)
				return nil
			}
			mu.Lock()
			allChunks = append(allChunks, chunks...)
			mu.Unlock()

			slog.Debug("channel returned results", "channel", ch.Name(), "count", len(chunks))
			return nil
		})
	}
	// 等待所有通道完成
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// 5. 应用后处理器链（按 Order 顺序）
	result := allChunks
	for _, pp := range e.postProcessors {
		var err error
		result, err = pp.Process(ctx, reqCtx.Query, result)
		if err != nil {
			// ⭐ 非致命错误：记录警告，继续使用上一步结果
			slog.Warn("post-processor failed", "order", pp.Order(), "err", err)
		}
	}
	// 6. 最终截断到 TopK
	if reqCtx != nil && reqCtx.TopK > 0 && len(result) > reqCtx.TopK {
		result = result[:reqCtx.TopK]
	}

	return result, nil
}
