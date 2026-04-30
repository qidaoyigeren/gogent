package guidance

import (
	"fmt"
	"gogent/internal/intent"
	"log/slog"
	"strings"
)

// Config 歧义引导配置
type Config struct {
	Enabled            bool    `mapstructure:"enabled"`             // 是否启用歧义引导
	AmbiguityThreshold float64 `mapstructure:"ambiguity-threshold"` // 歧义阈值（分数差值）
	MaxOptions         int     `mapstructure:"max-options"`         // 最大选项数量
}

// Service 歧义引导服务（5 步检测算法）
// 核心职责：
// 1. 检测意图结果是否存在歧义
// 2. 如果歧义严重，构建引导消息让用户选择
// 3. 返回决策结果（PROCEED/GUIDE/FALLBACK）
type Service struct {
	cfg Config
}

func NewService(cfg Config) *Service {
	if cfg.MaxOptions <= 0 {
		cfg.MaxOptions = 4
	}
	if cfg.AmbiguityThreshold <= 0 {
		cfg.AmbiguityThreshold = 0.3
	}
	return &Service{cfg: cfg}
}

// Evaluate checks if the intent results are ambiguous.
//
// 5-step algorithm:
// 1. If disabled or no intents → PROCEED
// 2. If single high-confidence intent → PROCEED
// 3. If multiple intents from different systems with similar names → GUIDE
// 4. If top scores are close (diff < threshold) → GUIDE
// 5. Otherwise → PROCEED
func (s *Service) Evaluate(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode) GuidanceDecision {
	if !s.cfg.Enabled {
		return GuidanceDecision{Type: DecisionProceed}
	}

	// Step 1: Collect all scores
	var allScores []scored
	for _, sq := range group.SubQuestions {
		for _, ns := range sq.Scores {
			allScores = append(allScores, scored{nodeID: ns.NodeID, score: ns.Score})
		}
	}

	if len(allScores) == 0 {
		return GuidanceDecision{Type: DecisionProceed}
	}

	// Step 2: Single high-confidence
	if len(allScores) == 1 && allScores[0].score >= 0.7 {
		return GuidanceDecision{Type: DecisionProceed}
	}

	// Step 3: Check for cross-system name collision
	if s.hasCrossSystemAmbiguity(allScores, nodeMap) {
		return s.buildGuidance(allScores, nodeMap)
	}

	// Step 4: Check score proximity
	if len(allScores) >= 2 {
		top := allScores[0].score
		second := allScores[1].score
		// Find actual top two
		for _, sc := range allScores {
			if sc.score > top {
				second = top
				top = sc.score
			} else if sc.score > second && sc.score < top {
				second = sc.score
			}
		}
		if top-second < s.cfg.AmbiguityThreshold {
			return s.buildGuidance(allScores, nodeMap)
		}
	}

	// Step 5: No ambiguity
	return GuidanceDecision{Type: DecisionProceed}
}

func (s *Service) hasCrossSystemAmbiguity(scores []scored, nodeMap map[string]*intent.IntentNode) bool {
	// 按节点名称分组，检查同名节点是否出现在不同父节点下
	nameToParents := make(map[string]map[string]bool)
	for _, sc := range scores {
		node, ok := nodeMap[sc.nodeID]
		if !ok {
			continue
		}
		if _, exists := nameToParents[node.Name]; !exists {
			nameToParents[node.Name] = make(map[string]bool)
		}
		nameToParents[node.Name][node.ParentID] = true
	}

	// 如果某个名称对应多个父节点 → 跨系统歧义
	for name, parents := range nameToParents {
		if len(parents) > 1 {
			slog.Info("cross-system ambiguity detected", "name", name, "parentCount", len(parents))
			return true
		}
	}
	return false
}

// scored 内部结构体：节点 ID + 分数
type scored struct {
	nodeID string
	score  float64
}

// buildGuidance 构建引导决策结果
// 工作流程：
// 1. 收集选项（最多 MaxOptions 个）
// 2. 构建选项文本（名称 + 描述）
// 3. 格式化选项列表（1. xxx \n 2. yyy）
// 4. 返回 GUIDE 决策
func (s *Service) buildGuidance(scores []scored, nodeMap map[string]*intent.IntentNode) GuidanceDecision {
	var options []string
	var optionNodeIDs []string
	seen := make(map[string]bool)

	// 收集选项（最多 MaxOptions 个，默认 4）
	for _, sc := range scores {
		if len(options) >= s.cfg.MaxOptions {
			break
		}
		node, ok := nodeMap[sc.nodeID]
		if !ok || seen[sc.nodeID] {
			continue
		}
		seen[sc.nodeID] = true

		// 构建选项文本：名称 + 描述（可选）
		label := node.Name
		if node.Description != "" {
			label += " (" + node.Description + ")"
		}
		options = append(options, label)
		optionNodeIDs = append(optionNodeIDs, node.ID)
	}

	// 格式化引导消息
	msg := fmt.Sprintf("%s，请选择您想了解的具体内容：\n%s",
		AmbiguityPromptPrefix,
		formatOptions(options))

	return GuidanceDecision{
		Type:          DecisionGuide,
		Message:       msg,
		Options:       options,
		OptionNodeIDs: optionNodeIDs,
	}
}

// formatOptions 格式化选项列表（1. xxx \n 2. yyy \n）
func formatOptions(options []string) string {
	var sb strings.Builder
	for i, opt := range options {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, opt))
	}
	return sb.String()
}
