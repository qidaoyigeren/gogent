package ingestion

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// PipelineNode 旧版顺序流水线接口：一个节点 = 对 *Document 的一次加工（兼容历史代码）。
type PipelineNode interface {
	Name() string
	Process(ctx context.Context, doc *Document) error
}

// NodeExecutor 新版单节点抽象：入参是共享的 IngestionContext + 本节点 NodeConfig。
type NodeExecutor interface {
	Name() string
	Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult
}

// Document 旧版流水线中的“文档”载体，字段由各个 PipelineNode 逐步填充；新链路优先用 IngestionContext。
type Document struct {
	ID        string
	SourceURL string
	FileName  string
	Content   string
	Parsed    string
	Chunks    []Chunk
	Metadata  map[string]string
	KBID      string
}

// Chunk 文段 + 可选向量 + 元数据快照；新链路中分块后写入 ctx.Chunks，与旧 Document.Chunks 同名概念。
type Chunk struct {
	ID       string
	Content  string
	Vector   []float32
	Metadata map[string]string
}

// Engine 旧版：持有 PipelineNode 切片，Run 时**严格按切片顺序**依次 Process，无条件分支、无 NodeConfig。
type Engine struct {
	nodes []PipelineNode
}

func NewEngine(nodes ...PipelineNode) *Engine {
	return &Engine{nodes: nodes}
}

func (e *Engine) Run(ctx context.Context, doc *Document) error {
	slog.Info("legacy ingestion pipeline started", "docID", doc.ID, "fileName", doc.FileName)
	for _, node := range e.nodes {
		// 任一节点返回错误则整条旧流水线失败，不维护 IngestionStatus 枚举，由调用方处理。
		if err := node.Process(ctx, doc); err != nil {
			return fmt.Errorf("pipeline node %s failed: %w", node.Name(), err)
		}
	}
	slog.Info("legacy ingestion pipeline completed", "docID", doc.ID, "chunks", len(doc.Chunks))
	return nil
}

// NodeRegistry 将「节点类型枚举」映射到**唯一**一个 NodeExecutor 实现；同类型后者覆盖前者。
type NodeRegistry struct {
	executors map[IngestionNodeType]NodeExecutor
}

// NewNodeRegistry 把各个节点实现注册成“节点类型 -> 执行器”的映射。
// pipeline 定义中只保存 nodeType 字符串，真正执行时需要通过这里找到对应的
// FetcherNode、ParserNode、ChunkerNode 或 IndexerNode。
func NewNodeRegistry(executors ...NodeExecutor) *NodeRegistry {
	registry := &NodeRegistry{executors: make(map[IngestionNodeType]NodeExecutor)}
	for _, executor := range executors {
		// nil 执行器直接忽略，允许 Runtime 在某些依赖缺失时仍能构造注册表。
		if executor == nil {
			continue
		}
		nodeType, err := ParseIngestionNodeType(executor.Name())
		if err != nil {
			continue
		}
		registry.executors[nodeType] = executor
	}
	return registry
}

// Register 允许测试或扩展代码手动替换某种节点类型的执行器。
func (r *NodeRegistry) Register(nodeType IngestionNodeType, executor NodeExecutor) {
	if r.executors == nil {
		r.executors = make(map[IngestionNodeType]NodeExecutor)
	}
	r.executors[nodeType] = executor
}

// Get 按 pipeline 节点配置中的 nodeType 找到实际执行器。
// 这里会先标准化枚举，避免大小写或横线/下划线差异导致节点无法执行。
func (r *NodeRegistry) Get(nodeType string) (NodeExecutor, error) {
	normalized, err := ParseIngestionNodeType(nodeType)
	if err != nil {
		return nil, err
	}
	executor, ok := r.executors[normalized]
	if !ok {
		return nil, fmt.Errorf("no executor registered for node type %s", normalized)
	}
	return executor, nil
}

// IngestionEngine 新版执行器：只依赖三个协作对象，不直接引用具体 fetcher/parser 类型。
type IngestionEngine struct {
	registry  *NodeRegistry        // 节点类型 -> 实现
	evaluator *ConditionEvaluator  // 每个 NodeConfig.Condition 的求值
	extractor *NodeOutputExtractor // 节点未显式给 Output 时，从 context 抽摘要供落库
}

// NewIngestionEngine 注入节点注册表、条件求值器和输出提取器。
// evaluator/extractor 允许传 nil，构造函数会补默认实现，方便测试只关注节点行为。
func NewIngestionEngine(registry *NodeRegistry, evaluator *ConditionEvaluator, extractor *NodeOutputExtractor) *IngestionEngine {
	if evaluator == nil {
		evaluator = NewConditionEvaluator()
	}
	if extractor == nil {
		extractor = NewNodeOutputExtractor()
	}
	return &IngestionEngine{
		registry:  registry,
		evaluator: evaluator,
		extractor: extractor,
	}
}

// Execute 执行一条 PipelineDefinition，并持续更新同一个 IngestionContext。
// 流程是：初始化上下文 -> 排序/校验节点 -> 条件判断 -> 执行节点 -> 记录日志 ->
// 根据节点结果决定继续、失败或提前终止。调用方通过返回的 context 读取最终文本、
// chunks、metadata、node logs 和错误状态。
func (e *IngestionEngine) Execute(ctx context.Context, pipeline PipelineDefinition, ingestCtx *IngestionContext) (*IngestionContext, error) {
	if ingestCtx == nil {
		ingestCtx = &IngestionContext{}
	}
	ingestCtx.Status = IngestionStatusRunning
	ingestCtx.EnsureMetadata() // 保证后续节点写 Metadata 不遇 nil map

	// 先把“乱序的节点数组 + nextNodeId 链”变成线性有序列表；失败说明配置不合法，整任务失败。
	orderedNodes, err := OrderPipelineNodes(pipeline.Nodes)
	if err != nil {
		ingestCtx.Status = IngestionStatusFailed
		ingestCtx.Error = err
		return ingestCtx, err
	}

	for idx, node := range orderedNodes {
		// condition 是节点级开关：未满足时不算失败，但必须写日志，便于任务详情
		// 解释为什么某个节点没有产出。
		if !e.evaluator.Evaluate(ingestCtx, node.Condition) {
			ingestCtx.AppendLog(NodeLog{
				NodeID:    node.NodeID,
				NodeType:  node.NodeType,
				NodeOrder: idx + 1,
				Message:   "Skipped: 条件未满足",
				Success:   true,
			})
			continue
		}

		// registry.Get 是 DSL 到代码实现的转换点：配置里写 "parser"，
		// 这里取到 ParserNode 并调用它的 Execute。
		executor, execErr := e.registry.Get(node.NodeType)
		if execErr != nil {
			ingestCtx.Status = IngestionStatusFailed
			ingestCtx.Error = execErr
			return ingestCtx, execErr
		}

		logEntry, result := e.runNode(ctx, executor, node, idx+1, ingestCtx)
		ingestCtx.AppendLog(logEntry)
		if !result.Success {
			// 节点失败后立即停止整条 pipeline，错误会同时保存在 context 和 task 表。
			ingestCtx.Status = IngestionStatusFailed
			ingestCtx.Error = result.Error
			return ingestCtx, result.Error
		}
		if !result.ShouldContinue {
			// terminate 类型结果：本节点成功但显式要求**不再**执行后续节点（如业务上已写完库）。
			break
		}
	}

	// 仅当未在中途因节点失败而置 Failed 时，标为完成；若上面 return 时已是 Failed 则保持。
	if ingestCtx.Status != IngestionStatusFailed {
		ingestCtx.Status = IngestionStatusCompleted
	}
	return ingestCtx, nil
}

// runNode 包装单个节点执行，统一采集耗时、输出、错误字符串。
// 节点可以显式返回 Output；如果没有返回，NodeOutputExtractor 会从 context 中
// 摘要出适合持久化的字段，避免把完整原文或向量直接写入日志。
func (e *IngestionEngine) runNode(
	ctx context.Context,
	executor NodeExecutor,
	node NodeConfig,
	order int,
	ingestCtx *IngestionContext,
) (NodeLog, NodeResult) {
	start := nowMillis()
	result := executor.Execute(ctx, ingestCtx, node) // 节点会**就地**修改 ingestCtx（追加 Chunks 等）
	duration := nowMillis() - start
	output := result.Output
	if output == nil {
		// 未自定义 Output 时，按节点类型从 context 抽“体量类”字段，避免把全文写入任务表。
		output = e.extractor.Extract(ingestCtx, node, result)
	}
	return NodeLog{
		NodeID:     node.NodeID,
		NodeType:   node.NodeType,
		NodeOrder:  order,
		Message:    result.Message,
		DurationMs: duration,
		Success:    result.Success,
		Error:      errorMessage(result.Error),
		Output:     output,
	}, result
}

// OrderPipelineNodes 将「乱序 + nextNodeId」整理成**线性可执行**列表；校验空/重复 id、断链、环，并处理多链头与孤立节点。
func OrderPipelineNodes(nodes []NodeConfig) ([]NodeConfig, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	nodeMap := make(map[string]NodeConfig, len(nodes))  // nodeId -> 配置
	referenced := make(map[string]struct{}, len(nodes)) // 被某 nextNodeId 指到的 id（不可能是链头）
	for _, node := range nodes {
		// nodeID 是 nextNodeId 连接图的主键，空值会让起点和环路判断失效。
		if strings.TrimSpace(node.NodeID) == "" {
			return nil, fmt.Errorf("pipeline node id cannot be empty")
		}
		if _, exists := nodeMap[node.NodeID]; exists {
			return nil, fmt.Errorf("duplicate pipeline node id: %s", node.NodeID)
		}
		normalizedType, err := ParseIngestionNodeType(node.NodeType)
		if err != nil {
			return nil, err
		}
		// 标准化后再存入 map，后续执行日志和 registry 查找保持同一套枚举值。
		node.NodeType = string(normalizedType)
		nodeMap[node.NodeID] = node
		if strings.TrimSpace(node.NextNodeID) != "" {
			referenced[node.NextNodeID] = struct{}{} // 被引用者：一定是某条链的“非起点”
		}
	}
	// 第二遍：保证每条 next 边指向的 nodeId 在 map 里存在，否则是悬空的 nextNodeId。
	for _, node := range nodes {
		if node.NextNodeID != "" {
			if _, ok := nodeMap[node.NextNodeID]; !ok {
				return nil, fmt.Errorf("pipeline node %s references missing next node %s", node.NodeID, node.NextNodeID)
			}
		}
	}

	// 链头 = 从没有任何人的 next 指向它的那些 nodeId；可能多个头（多条并行链从同一批节点定义里出现）。
	startNodes := make([]NodeConfig, 0)
	for _, node := range nodes {
		if _, ok := referenced[node.NodeID]; !ok {
			startNodes = append(startNodes, nodeMap[node.NodeID])
		}
	}
	if len(startNodes) == 0 {
		// 典型原因：全表节点形成环，导致每个点都“被引用”，没有链头。
		return nil, fmt.Errorf("pipeline has no start node")
	}

	ordered := make([]NodeConfig, 0, len(nodes))
	globalVisited := make(map[string]struct{}, len(nodes)) // 已按链顺序收进 ordered 的节点
	for _, start := range startNodes {
		localVisited := make(map[string]struct{}, len(nodes)) // 仅检测**当前**这条 walk 的环
		current := start
		for {
			if _, ok := localVisited[current.NodeID]; ok {
				return nil, fmt.Errorf("pipeline cycle detected at node %s", current.NodeID)
			}
			localVisited[current.NodeID] = struct{}{}
			if _, ok := globalVisited[current.NodeID]; ok {
				// 两条链汇合到同一后缀：第二个头 walk 到已访问节点时直接截断，避免重复追加。
				break
			}
			globalVisited[current.NodeID] = struct{}{}
			ordered = append(ordered, current)
			if strings.TrimSpace(current.NextNodeID) == "" {
				break // 本链到尾
			}
			current = nodeMap[current.NextNodeID]
		}
	}

	// 处理“孤立节点”：未出现在任何从链头 walk 的序列里（脏数据/历史配置），仍 append 以尽力执行。
	for _, node := range nodes {
		if _, ok := globalVisited[node.NodeID]; !ok {
			ordered = append(ordered, nodeMap[node.NodeID])
		}
	}
	return ordered, nil
}

// nowMillis 供节点耗时统计；与 Java 侧毫秒对齐。
func nowMillis() int64 {
	return time.Now().UnixMilli()
}
