package ingestion

// Runtime 离线流水线的**装配与入口**：内部持有一个已注册好全部 NodeExecutor 的 IngestionEngine，
// 对外只暴露 Execute 与默认 PipelineDefinition。业务只传 pipeline + context，不关心各节点如何 new。

import (
	"context"

	"gogent/internal/chat"
	"gogent/internal/embedding"
	internalparser "gogent/internal/parser"
	"gogent/internal/vector"
)

// Runtime 是入库流水线的依赖注入容器和执行协调器。
// 它把 embedding、vector、llm、tika 等外部服务和本地 parser 聚合成统一的 IngestionEngine。
//
// 核心职责：
// - NodeRegistry：注册 fetcher/parser/chunker/indexer 等节点的实现
// - DefaultPipelineDefinition：当 handler 未显式传 pipelineId 时提供内置链路
// - Execute：把用户请求转发给 engine，后者按节点顺序执行并更新 context
//
// 关键设计：节点实现和流水线定义完全解耦。handler 和 scheduler 都通过
// Runtime 执行，无需关心节点细节。如果需要新增或修改节点，只需在 NewRuntime
// 中改注册表或调整 DefaultPipelineDefinition 中的链接关系。
type Runtime struct {
	engine            *IngestionEngine // 执行引擎：按节点顺序执行，处理条件和错误
	defaultDimension  int              // 向量维度，indexer 节点用于 collection 创建和维度校验
	defaultCollection string           // 向量集合名，多个知识库若无专属向量空间则共享此集合
}

// NewRuntime 装配离线入库的运行时环境。
//
// 返回的 Runtime 预注册了 6 个节点：fetcher 取字节 → parser 文本 → chunker 分块 →
// enhancer LLM增强（可选）→ enricher 添元数据（可选）→ indexer 写向量。
// 用户也可通过 runtime.engine.registry.Register 替换或扩展节点实现。
func NewRuntime(
	embSvc embedding.EmbeddingService,
	vectorSvc vector.VectorStoreService,
	llmSvc chat.LLMService,
	tika *internalparser.TikaParser,
	defaultDimension int,
	defaultCollection string,
) *Runtime {
	// 注册顺序无关，map 以节点**类型**为键；后面的同类型会覆盖前面的（Register 同理）。
	registry := NewNodeRegistry(
		NewFetcherNode(),                 // 多源取 RawBytes
		NewParserNode(tika),              // 可为 nil tika 时仅内置 text/html/markdown
		NewEnhancerNode(llmSvc),          // 文档级 LLM 增强，可选条件执行
		NewChunkerNode(embSvc, 512, 128), // 默认 512/128 与配置中心可对齐
		NewEnricherNode(llmSvc),          // 块级 enrich
		NewIndexerNode(vectorSvc, defaultDimension, defaultCollection), // 写向量或仅准备数据视 Skip 标志
	)
	return &Runtime{
		engine:            NewIngestionEngine(registry, NewConditionEvaluator(), NewNodeOutputExtractor()),
		defaultDimension:  defaultDimension,  // 冗余保存：供 handler 与默认 indexer 外逻辑使用
		defaultCollection: defaultCollection, // 同上
	}
}

// Execute 直接委托 IngestionEngine，无额外业务；便于单测换 Engine 或打桩。
func (r *Runtime) Execute(ctx context.Context, pipeline PipelineDefinition, ingestCtx *IngestionContext) (*IngestionContext, error) {
	return r.engine.Execute(ctx, pipeline, ingestCtx)
}

// DefaultPipelineDefinition 最简四节点链，无 enhancer/enricher；RunRuntime 在 pipelineId 为空时用。
func (r *Runtime) DefaultPipelineDefinition() PipelineDefinition {
	return PipelineDefinition{
		ID:   "default-ingestion-pipeline",
		Name: "default-ingestion-pipeline",
		Nodes: []NodeConfig{
			{NodeID: "fetcher", NodeType: string(IngestionNodeTypeFetcher), NextNodeID: "parser"},
			{NodeID: "parser", NodeType: string(IngestionNodeTypeParser), NextNodeID: "chunker"},
			{NodeID: "chunker", NodeType: string(IngestionNodeTypeChunker), NextNodeID: "indexer"},
			{NodeID: "indexer", NodeType: string(IngestionNodeTypeIndexer)}, // 尾节点无 NextNodeID
		},
	}
}
