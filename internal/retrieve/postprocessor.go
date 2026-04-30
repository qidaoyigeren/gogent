package retrieve

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"gogent/internal/rerank"
	"log/slog"
	"sort"
	"strings"
)

// DeduplicationPostProcessor 去重后处理器
// 核心职责：
// 1. 移除重复的文档块（基于 ID 或内容哈希）
// 2. 保留高优先级通道的结果
// 3. 如果重复，保留分数更高的结果
//
// 去重策略：
// 1. 优先使用 ID 去重（如果 ID 非空）
// 2. 否则使用内容哈希去重（SHA-1，前 400 字符）
// 3. 按通道优先级排序（intent-directed > vector-global）
// 4. 如果遇到重复，保留分数更高的
//
// 执行顺序：Order() = 1（最先执行）
type DeduplicationPostProcessor struct{}

func NewDeduplicationPostProcessor() *DeduplicationPostProcessor {
	return &DeduplicationPostProcessor{}
}

// Order 返回执行顺序（1 = 最先执行，在 rerank 之前）
func (p *DeduplicationPostProcessor) Order() int { return 1 }

// Process 执行去重处理
//
// 工作流程：
// 1. 按通道优先级排序（低优先级通道在前）
// 2. 遍历所有 chunk，构建去重映射
// 3. 如果 chunk 已存在，保留分数更高的
// 4. 返回去重后的结果
//
// 为什么先去重再 Rerank？
// - 减少 Rerank 的输入数量，节省成本
// - 避免对重复内容进行多次评分
// - 提高 Rerank 的准确性（无重复干扰）
func (p *DeduplicationPostProcessor) Process(_ context.Context, _ string, chunks []DocumentChunk) ([]DocumentChunk, error) {
	seen := make(map[string]int) // key -> result 中的索引
	var result []DocumentChunk

	// 1. 按通道优先级排序（数字越小优先级越高）
	sort.Slice(chunks, func(i, j int) bool {
		return channelPriority(chunks[i].ChannelName) < channelPriority(chunks[j].ChannelName)
	})

	// 2. 去重
	for _, chunk := range chunks {
		key := chunkKey(chunk)
		if idx, exists := seen[key]; !exists {
			// 首次出现，添加到结果
			seen[key] = len(result)
			result = append(result, chunk)
		} else {
			// 重复出现，保留分数更高的
			if chunk.Score > result[idx].Score {
				result[idx] = chunk
			}
		}
	}

	slog.Debug("dedup", "before", len(chunks), "after", len(result))
	return result, nil
}

// chunkKey 生成 chunk 的唯一标识
// 优先级：
// 1. 使用 ID（如果非空）
// 2. 使用内容哈希（SHA-1，前 400 字符）
func chunkKey(c DocumentChunk) string {
	// 优先使用 ID
	if strings.TrimSpace(c.ID) != "" {
		return "id:" + c.ID
	}

	// 使用内容哈希（截断到 400 字符，避免过长）
	text := strings.TrimSpace(c.Content)
	if len(text) > 400 {
		text = text[:400]
	}
	sum := sha1.Sum([]byte(text))
	return "c:" + hex.EncodeToString(sum[:])
}

// channelPriority 返回通道的去重优先级
// 数字越小，优先级越高（越容易保留）
func channelPriority(name string) int {
	switch name {
	case "intent-directed":
		return 1 // 最高优先级（意图定向）
	case "vector-global":
		return 10 // 中等优先级（全局向量）
	default:
		return 50 // 最低优先级（其他通道）
	}
}

// RerankPostProcessor Rerank 后处理器
// 核心职责：
// 1. 使用 Rerank 模型对检索结果重新排序
// 2. 提高结果的相关性准确性
// 3. Rerank 失败时保留原始顺序（降级）
//
// 执行顺序：Order() = 10（在 dedup 之后执行）
type RerankPostProcessor struct {
	rerankSvc rerank.RerankService // Rerank 服务
}

func NewRerankPostProcessor(rerankSvc rerank.RerankService) *RerankPostProcessor {
	return &RerankPostProcessor{rerankSvc: rerankSvc}
}

// Order 返回执行顺序（10 = 在 dedup 之后）
func (p *RerankPostProcessor) Order() int { return 10 }

// Process 执行 Rerank 处理
//
// 工作流程：
// 1. 如果只有 0-1 个结果，直接返回（无需 Rerank）
// 2. 提取所有 chunk 的文本内容
// 3. 调用 Rerank 服务重新打分
// 4. 按 Rerank 分数重新排序
// 5. 更新 chunk 的分数为 Rerank 分数
//
// 容错策略：
// - Rerank 失败：非致命，记录警告，返回原始结果
// - 这样可以保证即使 Rerank 服务不可用，检索仍然可用
func (p *RerankPostProcessor) Process(ctx context.Context, query string, chunks []DocumentChunk) ([]DocumentChunk, error) {
	// 1. 检查是否需要 Rerank
	if len(chunks) <= 1 {
		return chunks, nil // 0-1 个结果无需 Rerank
	}

	// 2. 提取文本内容
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	// 3. 调用 Rerank 服务
	// 请求所有结果重新排序（最终 TopK 截断由 engine 处理）
	results, err := p.rerankSvc.Rerank(ctx, query, texts, len(chunks))
	if err != nil {
		// ⭐ 非致命错误：记录警告，返回原始结果
		slog.Warn("rerank failed, keeping original order", "err", err)
		return chunks, nil
	}

	// 4. 按 Rerank 分数重新排序
	reranked := make([]DocumentChunk, 0, len(results))
	for _, r := range results {
		if r.Index < len(chunks) {
			chunk := chunks[r.Index]
			chunk.Score = r.Score // 更新分数为 Rerank 分数
			reranked = append(reranked, chunk)
		}
	}

	return reranked, nil
}
