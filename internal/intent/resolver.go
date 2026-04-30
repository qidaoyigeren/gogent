package intent

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"
)

// Resolver 意图解析器（并行处理多个子问题）
type Resolver struct {
	classifier *DefaultClassifier // 分类器
	maxIntents int                // 最大意图数量上限
}

func NewResolver(classifier *DefaultClassifier, maxIntents int) *Resolver {
	if maxIntents <= 0 {
		maxIntents = MaxIntentCount
	}
	return &Resolver{
		classifier: classifier,
		maxIntents: maxIntents,
	}
}

// Resolve 并行分类多个子问题
// 工作流程：
// 1. 收集所有叶子节点作为候选
// 2. 并行分类每个子问题（errgroup）
// 3. 聚合结果到 IntentGroup
// 4. 限制总意图数量
func (r *Resolver) Resolve(ctx context.Context, subQuestions []string, roots []*IntentNode) (*IntentGroup, error) {
	// 空子问题或空根节点 → 返回空结果
	if len(subQuestions) == 0 || len(roots) == 0 {
		return &IntentGroup{}, nil
	}

	// 收集所有叶子节点作为候选（用于 LLM 分类）
	candidates := collectClassifyCandidates(roots)

	if len(candidates) == 0 {
		return &IntentGroup{}, nil
	}

	// 并行分类每个子问题
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	results := make([]SubQuestionIntent, len(subQuestions))

	for i, q := range subQuestions {
		i, q := i, q // 捕获循环变量（goroutine 安全）
		g.Go(func() error {
			// 调用分类器
			scores, err := r.classifier.Classify(gCtx, q, candidates)
			if err != nil {
				// 分类失败 → 非致命错误，记录警告
				slog.Warn("classification failed for sub-question", "question", q, "err", err)
				return nil
			}

			// 存储结果（互斥锁保护）
			mu.Lock()
			results[i] = SubQuestionIntent{
				Question: q,
				Scores:   scores,
			}
			mu.Unlock()
			return nil
		})
	}

	// 等待所有 goroutine 完成
	if err := g.Wait(); err != nil {
		return nil, err
	}

	group := &IntentGroup{SubQuestions: results}

	// 限制总意图数量
	r.capTotalIntents(group)

	return group, nil
}

// collectClassifyCandidates 收集所有启用的叶子节点（用于 LLM 分类）
// 对齐 Java DefaultIntentClassifier：收集整棵树的所有叶子，而不仅是根节点的直接子节点
func collectClassifyCandidates(roots []*IntentNode) []*IntentNode {
	// 收集所有根节点下的叶子节点
	seen := make(map[string]bool)
	var out []*IntentNode
	for _, root := range roots {
		if root == nil {
			continue
		}
		for _, leaf := range root.AllLeaves() {
			// 过滤：非空、启用、有 ID、未重复
			if leaf == nil || !leaf.Enabled || leaf.ID == "" {
				continue
			}
			if seen[leaf.ID] {
				continue
			}
			seen[leaf.ID] = true
			out = append(out, leaf)
		}
	}
	return out
}

// intentScoreRef 意图分数引用（用于排序和截断）
type intentScoreRef struct {
	sqIdx int     // 子问题索引
	sIdx  int     // 分数索引
	score float64 // 分数
}

// capTotalIntents 限制总意图数量（对齐 Java IntentResolver#capTotalIntents）
// 策略：
// 1. 每个子问题保留最高分意图（保证每个子问题至少有 1 个结果）
// 2. 剩余槽位按全局分数排序分配
func (r *Resolver) capTotalIntents(group *IntentGroup) {
	// 收集所有意图分数引用
	var refs []intentScoreRef
	for i, sq := range group.SubQuestions {
		for j, ns := range sq.Scores {
			refs = append(refs, intentScoreRef{sqIdx: i, sIdx: j, score: ns.Score})
		}
	}

	// 如果总数未超过上限，无需截断
	if len(refs) <= r.maxIntents {
		return
	}

	// 按分数降序排序
	sort.Slice(refs, func(a, b int) bool {
		return refs[a].score > refs[b].score
	})

	// 第一步：保证每个子问题至少有 1 个最高分意图
	nSub := len(group.SubQuestions)
	guaranteed := make([]intentScoreRef, 0, nSub)
	chosenSq := make(map[int]bool)
	for _, rf := range refs {
		if chosenSq[rf.sqIdx] {
			continue
		}
		chosenSq[rf.sqIdx] = true
		guaranteed = append(guaranteed, rf)
		if len(guaranteed) == nSub {
			break
		}
	}

	// 第二步：分配剩余槽位
	remaining := r.maxIntents - len(guaranteed)
	if remaining <= 0 {
		applyIntentCapRefs(group, guaranteed)
		return
	}

	// 收集未被选中的意图
	gset := make(map[[2]int]bool)
	for _, g := range guaranteed {
		gset[[2]int{g.sqIdx, g.sIdx}] = true
	}
	var addl []intentScoreRef
	for _, rf := range refs {
		if gset[[2]int{rf.sqIdx, rf.sIdx}] {
			continue
		}
		addl = append(addl, rf)
		if len(addl) >= remaining {
			break
		}
	}

	// 应用截断
	applyIntentCapRefs(group, append(guaranteed, addl...))
}

// applyIntentCapRefs 应用截断结果，保留被选中的意图
func applyIntentCapRefs(group *IntentGroup, picks []intentScoreRef) {
	keep := make(map[[2]int]bool)
	for _, p := range picks {
		keep[[2]int{p.sqIdx, p.sIdx}] = true
	}
	for i := range group.SubQuestions {
		var out []NodeScore
		for j, s := range group.SubQuestions[i].Scores {
			if keep[[2]int{i, j}] {
				out = append(out, s)
			}
		}
		group.SubQuestions[i].Scores = out
	}
}
