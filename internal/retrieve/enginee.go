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
//
// 工作流程：
// 1. 过滤启用的通道
// 2. 按优先级排序
// 3. 并行执行各通道搜索
// 4. 合并所有结果
// 5. 应用后处理器链
// 6. 截断到 TopK
type MultiChannelEngine struct {
	channels       []SearchChannel // 检索通道列表
	postProcessors []PostProcessor // 后处理器列表（按 Order 排序）
}

// NewMultiChannelEngine 创建多通道检索引擎
// 参数：
//   - channels: 检索通道列表
//   - processors: 后处理器列表
//
// 注意：
// - 后处理器会自动按 Order() 排序
// - 通道不会排序（保持传入顺序，由 engine.Retrieve 动态排序）
func NewMultiChannelEngine(channels []SearchChannel, processors []PostProcessor) *MultiChannelEngine {
	// 按 Order 排序后处理器
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
// 这是检索引擎的核心方法
//
// 工作流程：
// 1. 过滤启用的通道（调用 IsEnabled）
// 2. 按优先级排序（数字越小优先级越高）
// 3. 使用 errgroup 并行执行各通道搜索
// 4. 合并所有通道结果（使用互斥锁保护）
// 5. 依次应用后处理器（dedup → rerank）
// 6. 最终截断到 TopK
//
// 容错策略：
// - 通道失败：非致命，记录警告日志，继续其他通道
// - 后处理器失败：非致命，记录警告日志，使用上一步结果
// - 无启用通道：返回 nil，不报错
func (e *MultiChannelEngine) Retrieve(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	// 1. 过滤启用的通道
	var enabled []SearchChannel
	for _, ch := range e.channels {
		if ch.IsEnabled(ctx, reqCtx) {
			enabled = append(enabled, ch)
		}
	}

	// 2. 按优先级排序（升序，数字小 = 优先级高）
	// 对齐 Java MultiChannelRetrievalEngine 的行为
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
	var mu sync.Mutex             // 保护 allChunks 的并发写入
	var allChunks []DocumentChunk // 聚合所有通道结果

	for _, ch := range enabled {
		ch := ch // 捕获循环变量
		g.Go(func() error {
			slog.Debug("executing search channel", "channel", ch.Name(), "priority", ch.Priority())

			// 执行通道搜索
			chunks, err := ch.Search(gCtx, reqCtx)
			if err != nil {
				// ⭐ 非致命错误：记录警告，返回 nil（不中断其他通道）
				slog.Warn("channel search failed", "channel", ch.Name(), "err", err)
				return nil
			}

			// 合并结果（需要加锁）
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
	// 对齐 Java 引擎：后处理器产生最终 TopK 结果
	if reqCtx != nil && reqCtx.TopK > 0 && len(result) > reqCtx.TopK {
		result = result[:reqCtx.TopK]
	}

	return result, nil
}

// RetrieveBySubQuestions 基于子问题的检索
// 对齐 Java RetrievalEngine 的编排逻辑
//
// 工作流程：
// 1. 如果没有子问题，使用原始查询
// 2. 每个子问题独立执行完整的检索流程（Retrieve）
// 3. 并行执行所有子问题的检索
// 4. 合并所有子问题的结果
//
// 应用场景：
// - 查询重写后产生多个子问题
// - 每个子问题可能命中不同的知识库
// - 合并结果提高召回率
//
// 注意：
// - 合并后的结果没有去重（需要上层调用 postprocessor）
// - 某个子问题失败会中断整个流程（与 Retrieve 不同）
func (e *MultiChannelEngine) RetrieveBySubQuestions(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	// 1. 检查请求上下文
	if reqCtx == nil {
		return nil, nil
	}

	// 2. 确定查询列表
	questions := reqCtx.SubQuestions
	if len(questions) == 0 {
		// 降级：使用原始查询
		questions = []string{reqCtx.Query}
	}

	// 3. 并行执行每个子问题的检索
	g, gCtx := errgroup.WithContext(ctx)
	results := make([][]DocumentChunk, len(questions))

	for i, q := range questions {
		i, q := i, q // 捕获循环变量
		g.Go(func() error {
			// 创建子请求（复制上下文）
			subReq := *reqCtx
			subReq.Query = q
			subReq.SubQuestions = nil // 避免递归

			// 执行检索
			chunks, err := e.Retrieve(gCtx, &subReq)
			if err != nil {
				return err // ⭐ 子问题失败会中断整个流程
			}
			results[i] = chunks
			return nil
		})
	}

	// 4. 等待所有子问题完成
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// 5. 合并所有子问题的结果
	var merged []DocumentChunk
	for _, chunks := range results {
		merged = append(merged, chunks...)
	}

	return merged, nil
}
