package intent

// NodeScore 意图节点评分
type NodeScore struct {
	NodeID     string  `json:"nodeId"`     // 意图节点 ID
	Score      float64 `json:"score"`      // 分类置信度（0-1）
	Confidence string  `json:"confidence"` // 置信度等级：HIGH(>=0.8), MEDIUM(0.5-0.8), LOW(<0.5)
}

// SubQuestionIntent 子问题的意图结果
type SubQuestionIntent struct {
	Question string      `json:"question"` // 子问题原文
	Scores   []NodeScore `json:"scores"`   // 意图评分列表
}

// IntentGroup 所有子问题的意图聚合结果
type IntentGroup struct {
	SubQuestions []SubQuestionIntent `json:"subQuestions"` // 子问题意图列表
}

// AllNodeIDs 返回所有分数 >= minScore 的唯一意图节点 ID
func (g *IntentGroup) AllNodeIDs(minScore float64) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, sq := range g.SubQuestions {
		for _, s := range sq.Scores {
			if s.Score >= minScore && !seen[s.NodeID] {
				seen[s.NodeID] = true
				ids = append(ids, s.NodeID)
			}
		}
	}
	return ids
}

// TopScore 返回所有子问题中的最高分数
func (g *IntentGroup) TopScore() float64 {
	best := 0.0
	for _, sq := range g.SubQuestions {
		for _, s := range sq.Scores {
			if s.Score > best {
				best = s.Score
			}
		}
	}
	return best
}

// IntentClassifier 意图分类器接口
type IntentClassifier interface {
	// Classify 对查询进行分类，返回评分节点列表
	Classify(ctx interface{}, query string, candidates []*IntentNode) ([]NodeScore, error)
}
