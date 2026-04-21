package intent

import (
	"context"
	"fmt"
	"gogent/internal/chat"
	"gogent/pkg/llmutil"
	"log/slog"
	"sort"
	"strings"
)

// DefaultClassifier 基于 LLM 的默认意图分类器
type DefaultClassifier struct {
	llm chat.LLMService // LLM 服务
}

func NewDefaultClassifier(llm chat.LLMService) *DefaultClassifier {
	return &DefaultClassifier{llm: llm}
}

const classifyPrompt = `你是一个意图分类器。请分析用户问题，判断它最可能属于以下哪些分类。

候选分类列表：
%s

用户问题：%s

请以JSON数组格式返回分类结果，每个元素包含 nodeId 和 score（0-1之间的置信度）：
[{"nodeId": "xxx", "score": 0.85, "confidence": "HIGH"}, ...]

规则：
1. score >= 0.8 为 HIGH，0.5-0.8 为 MEDIUM，< 0.5 为 LOW
2. 只返回 score >= 0.35 的结果
3. 最多返回3个结果
4. 确保 nodeId 严格匹配候选列表中的 id`

// Classify 对查询进行意图分类
func (c *DefaultClassifier) Classify(ctx context.Context, query string, candidates []*IntentNode) ([]NodeScore, error) {
	//空候选列表->直接返回
	if len(candidates) == 0 {
		return nil, nil
	}
	// 构建候选节点描述（名称 + 描述 + 关键词）
	var sb strings.Builder
	for _, node := range candidates {
		desc := node.Name
		if node.Description != "" {
			desc += " - " + node.Description
		}
		if len(node.Keywords) > 0 {
			desc += " (关键词: " + strings.Join(node.Keywords, ", ") + ")"
		}
		sb.WriteString(fmt.Sprintf("- id: %s, name: %s\n", node.ID, desc))
	}
	// 调用 LLM（低温度保证分类稳定性）
	prompt := fmt.Sprintf(classifyPrompt, sb.String(), query)
	resp, err := c.llm.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, chat.WithTemperature(0.1))
	if err != nil {
		return nil, fmt.Errorf("classify LLM failed %w", err)
	}
	// 提取思考内容并解析 JSON
	content := llmutil.ExtractThinkContent(resp.Content)
	scores, parseErr := llmutil.ParseJSON[[]NodeScore](content)
	if parseErr != nil {
		slog.Warn("failed to parse classification", "err", parseErr, "raw", content)
		return nil, nil
	}
	// 过滤有效的节点 ID（确保 nodeId 在候选列表中）
	validIDs := make(map[string]bool)
	for _, node := range candidates {
		validIDs[node.ID] = true
	}
	// 过滤有效分数（节点 ID 有效 && 分数 >= 最低阈值）
	var validScores []NodeScore
	for _, s := range scores {
		if validIDs[s.NodeID] && s.Score >= IntentMinScore {
			validScores = append(validScores, s)
		}
	}
	// 按分数降序排序
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].Score > scores[j].Score
	})
	// 截断到最大意图数量（3个）
	if len(validScores) > MaxIntentCount {
		validScores = validScores[:MaxIntentCount]
	}

	return validScores, nil
}
