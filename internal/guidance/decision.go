package guidance

// DecisionType 歧义引导决策类型
type DecisionType int

const (
	DecisionProceed  DecisionType = iota // 无歧义，继续检索流程
	DecisionGuide                        // 有歧义，询问用户澄清
	DecisionFallback                     // 歧义严重，使用兜底策略
)

func (d DecisionType) String() string {
	switch d {
	case DecisionProceed:
		return "PROCEED"
	case DecisionGuide:
		return "GUIDE"
	case DecisionFallback:
		return "FALLBACK"
	default:
		return "UNKNOWN"
	}
}

// GuidanceDecision 歧义引导决策结果
type GuidanceDecision struct {
	Type          DecisionType
	Message       string   // 引导消息（仅当 Type == DecisionGuide 时有效）
	Options       []string // 候选选项列表
	OptionNodeIDs []string // 选项对应的意图节点 ID（与 Options 平行，数字 1..n）
}
