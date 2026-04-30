package ingestion

import "fmt"

//output_extractor.go 负责把「执行后的 IngestionContext 状态」压缩成**轻量级的摘要 JSON**，

type NodeOutputExtractor struct{}

// NewNodeOutputExtractor 创建节点输出摘要器。
func NewNodeOutputExtractor() *NodeOutputExtractor {
	return &NodeOutputExtractor{}
}

// Extract 从 IngestionContext 中按节点类型提取适合日志展示的摘要。
//
// 设计原则：
//  1. 只记录轻量级指标（大小、数量、类型、名称），避免大对象序列化到日志表
//  2. 不同节点关注不同维度：fetcher 关注来源和字节数，chunker 关注分块数等
//  3. result.Message 作为执行结果说明，metadata 全量附加（但有膨胀风险）
//  4. 未识别的节点类型也记录一条基础信息，避免 task_node 行完全为空
func (e *NodeOutputExtractor) Extract(ctx *IngestionContext, node NodeConfig, result NodeResult) map[string]interface{} {
	output := map[string]interface{}{}
	switch normalizeEnumValue(node.NodeType) {
	case string(IngestionNodeTypeFetcher):
		// Fetcher 节点：关注数据来源和原始字节大小，用于诊断抓取是否成功。
		// 如果 rawBytes=0 说明数据源读取失败或文件为空。
		output["sourceType"] = ctx.Source.Type
		output["sourceLocation"] = ctx.Source.Location
		output["fileName"] = ctx.FileName
		output["mimeType"] = ctx.MimeType
		output["rawBytes"] = len(ctx.RawBytes)
	case string(IngestionNodeTypeParser):
		// Parser 节点：关注解析后文本长度，用于判断 Tika/内置解析器是否成功提取正文。
		// rawTextSize=0 可能意味着解析失败或文件无文本内容（如纯图片）。
		output["mimeType"] = ctx.MimeType
		output["rawTextSize"] = len(ctx.RawText)
	case string(IngestionNodeTypeEnhancer):
		// Enhancer 节点（文档级 LLM 增强）：记录增强文本大小、关键词和问题数量。
		// 这些是 LLM 增强效果的核心观察点，用于评估增强质量。
		output["enhancedTextSize"] = len(ctx.EnhancedText)
		output["keywords"] = ctx.Keywords
		output["questions"] = ctx.Questions
	case string(IngestionNodeTypeChunker):
		// Chunker 节点：核心结果是分块数量，用于判断分块策略是否合理。
		// chunkCount=0 说明文本为空或分块参数配置错误。
		output["chunkCount"] = len(ctx.Chunks)
	case string(IngestionNodeTypeEnricher):
		// Enricher 节点（块级 LLM 增强）：同样以分块数作为主要摘要。
		// 可与 chunker 的 chunkCount 对比，验证 enricher 是否处理了所有块。
		output["chunkCount"] = len(ctx.Chunks)
	case string(IngestionNodeTypeIndexer):
		// Indexer 节点：记录分块数和向量空间信息，用于追溯写入了哪个 collection/namespace。
		// 如果向量库写入失败，可从这里快速定位目标空间。
		output["chunkCount"] = len(ctx.Chunks)
		if ctx.VectorSpaceID != nil {
			output["collectionName"] = ctx.VectorSpaceID.LogicalName
			output["namespace"] = ctx.VectorSpaceID.Namespace
		}
	}
	if result.Message != "" {
		output["message"] = result.Message
	}
	if len(ctx.Metadata) > 0 {
		output["metadata"] = ctx.Metadata
	}
	if len(output) == 0 {
		// 未识别 nodeType 或未命中 switch 分支：仍记录基础信息，
		// 避免 t_ingestion_task_node 表的 output_data 字段完全为空，保持审计完整性。
		return map[string]interface{}{"message": fmt.Sprintf("%s executed", node.NodeType)}
	}
	return output
}
