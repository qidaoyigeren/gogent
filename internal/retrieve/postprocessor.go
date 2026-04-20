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
type DeduplicationPostProcessor struct{}

func NewDeduplicationPostProcessor() *DeduplicationPostProcessor {
	return &DeduplicationPostProcessor{}
}

func (p *DeduplicationPostProcessor) Order() int { return 1 }

func (p *DeduplicationPostProcessor) Process(_ context.Context, _ string, chunks []DocumentChunk) ([]DocumentChunk, error) {
	var results []DocumentChunk
	seen := make(map[string]int)
	// 1. 按通道优先级排序（数字越小优先级越高）
	sort.Slice(chunks, func(i, j int) bool {
		return channelPriority(chunks[i].ChannelName) < channelPriority(chunks[j].ChannelName)
	})
	// 2. 去重
	for index, chunk := range chunks {
		key := chunkKey(chunk)
		if _, ok := seen[key]; !ok {
			seen[key] = index
			results = append(results, chunk)
		} else {
			//重复出现，保留分数高的
			if chunk.Score > chunks[seen[key]].Score {
				results[seen[key]] = chunk
				seen[key] = index
			}
		}
	}
	slog.Debug("dedup", "before", len(chunks), "after", len(results))
	return results, nil
}

// chunkKey 生成 chunk 的唯一标识
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

func channelPriority(name string) int {
	switch name {
	case "intent-directed":
		return 1
	case "vector-global":
		return 10
	default:
		return 50
	}
}

type RerankPostProcessor struct {
	rerankSvc rerank.RerankService // Rerank 服务
}

func NewRerankPostProcessor(rerankSvc rerank.RerankService) *RerankPostProcessor {
	return &RerankPostProcessor{rerankSvc: rerankSvc}
}

// Order 返回执行顺序（10 = 在 dedup 之后）
func (p *RerankPostProcessor) Order() int { return 10 }

func (p *RerankPostProcessor) Process(ctx context.Context, query string, chunks []DocumentChunk) ([]DocumentChunk, error) {
	if len(chunks) <= 1 {
		return chunks, nil
	}
	//提取文本内容
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}
	//调用Rerank服务
	results, err := p.rerankSvc.Rerank(ctx, query, texts, len(chunks))
	if err != nil {
		slog.Warn("rerank failed", "err", err)
		return nil, err
	}
	reranked := make([]DocumentChunk, 0, len(results))
	for _, r := range results {
		if r.Index < len(chunks) {
			chunk := chunks[r.Index]
			chunk.Score = r.Score
			reranked = append(reranked, chunk)
		}
	}
	return reranked, nil
}
