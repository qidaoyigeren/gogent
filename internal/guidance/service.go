package guidance

import (
	"fmt"
	"gogent/internal/intent"
	"log/slog"
	"strings"
)

type Config struct {
	Enabled            bool    `mapstructure:"enabled"`             // 是否启用歧义引导
	AmbiguityThreshold float64 `mapstructure:"ambiguity-threshold"` // 歧义阈值（分数差值）
	MaxOptions         int     `mapstructure:"max-options"`         // 最大选项数量
}

// Service 歧义引导服务（5 步检测算法）
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

// scored 内部结构体：节点 ID + 分数
type scored struct {
	nodeID string
	score  float64
}

// Evaluate 检查意图结果是否存在歧义。
//
// 5步判断算法：
// 1. 如果功能被禁用或没有意图结果 → 继续执行
// 2. 如果存在单个高置信度的意图 → 继续执行
// 3. 如果存在来自不同系统的多个意图且名称相似 → 引导用户澄清
// 4. 如果最高分之间的差距较小（差值小于阈值） → 引导用户澄清
// 5. 其他情况 → 继续执行
func (s *Service) Evaluate(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode) GuidanceDecision {
	if !s.cfg.Enabled {
		return GuidanceDecision{Type: DecisionProceed}
	}

	//收集所有scores
	var allScores []scored
	for _, sq := range group.SubQuestions {
		for _, ns := range sq.Scores {
			allScores = append(allScores, scored{nodeID: ns.NodeID, score: ns.Score})
		}
	}
	//没有意图结果
	if len(allScores) == 0 {
		return GuidanceDecision{Type: DecisionProceed}
	}
	//单个高置信度的意图
	if len(allScores) == 1 && allScores[0].score >= 0.7 {
		return GuidanceDecision{Type: DecisionProceed}
	}
	//如果存在来自不同系统的多个意图且名称相似
	if s.hasCrossSystemAmbiguity(allScores, nodeMap) {
		return s.buildGuidance(allScores, nodeMap)
	}
	//如果最高分之间的差距较小（差值小于阈值）
	if len(allScores) >= 2 {
		top := allScores[0].score
		second := allScores[1].score
		for _, sc := range allScores {
			if sc.score > top {
				second = top
				top = sc.score
			} else if sc.score > second {
				second = sc.score
			}
		}
		if top-second <= s.cfg.AmbiguityThreshold {
			return s.buildGuidance(allScores, nodeMap)
		}
	}
	//其他情况 → 继续执行
	return GuidanceDecision{Type: DecisionProceed}
}

func (s *Service) hasCrossSystemAmbiguity(scores []scored, nodeMap map[string]*intent.IntentNode) bool {
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
	for name, parents := range nameToParents {
		if len(parents) > 1 {
			slog.Info("cross-system ambiguity detected", "name", name, "parentCount", len(parents))
			return true
		}
	}
	return false
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

	for _, sc := range scores {
		if len(options) >= s.cfg.MaxOptions {
			break
		}
		node, ok := nodeMap[sc.nodeID]
		if !ok || seen[sc.nodeID] {
			continue
		}
		seen[sc.nodeID] = true

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
