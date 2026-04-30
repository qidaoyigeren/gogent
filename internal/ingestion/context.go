package ingestion

// 本文件定义一次入库执行过程中共享的“上下文协议”。
// SourceType、IngestionStatus、IngestionNodeType 等枚举约束外部输入；
// IngestionContext 保存原始字节、解析文本、分块、向量空间、元数据和节点日志；
// NodeConfig 描述 pipeline 中单个节点的配置与下一跳；NodeResult 统一表达节点
// 成功、失败、跳过或终止。engine、task_service、handler 和 scheduler 都围绕
// 这些结构交换状态，因此这里是理解 Day14 离线链路的核心数据模型。

import (
	"encoding/json"
	"fmt"
	"strings"
)

type SourceType string

const (
	SourceTypeFile   SourceType = "file"
	SourceTypeURL    SourceType = "url"
	SourceTypeFeishu SourceType = "feishu"
	SourceTypeS3     SourceType = "s3"
)

func ParseSourceType(value string) (SourceType, error) {
	switch normalizeEnumValue(value) {
	case "", string(SourceTypeFile):
		// 空串与 file 等价：本地路径或已上传落盘的文件
		return SourceTypeFile, nil
	case string(SourceTypeURL):
		return SourceTypeURL, nil
	case string(SourceTypeFeishu):
		return SourceTypeFeishu, nil
	case string(SourceTypeS3):
		return SourceTypeS3, nil
	default:
		return "", fmt.Errorf("unknown source type: %s", value)
	}
}

type IngestionStatus string

const (
	IngestionStatusPending   IngestionStatus = "pending"
	IngestionStatusRunning   IngestionStatus = "running"
	IngestionStatusFailed    IngestionStatus = "failed"
	IngestionStatusCompleted IngestionStatus = "completed"
)

func ParseIngestionStatus(value string) (IngestionStatus, error) {
	switch normalizeEnumValue(value) {
	case "", string(IngestionStatusPending):
		return IngestionStatusPending, nil
	case string(IngestionStatusRunning):
		// 任务或文档正在执行 pipeline
		return IngestionStatusRunning, nil
	case string(IngestionStatusFailed):
		return IngestionStatusFailed, nil
	case string(IngestionStatusCompleted):
		return IngestionStatusCompleted, nil
	default:
		return "", fmt.Errorf("unknown ingestion status: %s", value)
	}
}

type IngestionNodeType string

const (
	IngestionNodeTypeFetcher  IngestionNodeType = "fetcher"
	IngestionNodeTypeParser   IngestionNodeType = "parser"
	IngestionNodeTypeEnhancer IngestionNodeType = "enhancer"
	IngestionNodeTypeChunker  IngestionNodeType = "chunker"
	IngestionNodeTypeEnricher IngestionNodeType = "enricher"
	IngestionNodeTypeIndexer  IngestionNodeType = "indexer"
)

// ParseIngestionNodeType 解析节点类型；先经 normalizeEnumValue 统一大小写与连字符，再与预置枚举匹配。
func ParseIngestionNodeType(value string) (IngestionNodeType, error) {
	switch normalizeEnumValue(value) {
	case string(IngestionNodeTypeFetcher):
		return IngestionNodeTypeFetcher, nil
	case string(IngestionNodeTypeParser):
		return IngestionNodeTypeParser, nil
	case string(IngestionNodeTypeEnhancer):
		return IngestionNodeTypeEnhancer, nil
	case string(IngestionNodeTypeChunker):
		return IngestionNodeTypeChunker, nil
	case string(IngestionNodeTypeEnricher):
		return IngestionNodeTypeEnricher, nil
	case string(IngestionNodeTypeIndexer):
		return IngestionNodeTypeIndexer, nil
	default:
		return "", fmt.Errorf("unknown node type: %s", value)
	}
}

type NodeExecutionStatus string

const (
	NodeExecutionStatusOK        NodeExecutionStatus = "ok"
	NodeExecutionStatusSkip      NodeExecutionStatus = "skip"
	NodeExecutionStatusFail      NodeExecutionStatus = "fail"
	NodeExecutionStatusTerminate NodeExecutionStatus = "terminate"
)

type DocumentSource struct {
	Type        SourceType        `json:"type"`
	Location    string            `json:"location,omitempty"`
	FileName    string            `json:"fileName,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}

type StructuredDocument struct {
	Content  string                 `json:"content"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type VectorSpaceID struct {
	LogicalName string `json:"logicalName"`
	Namespace   string `json:"namespace,omitempty"`
}

type PipelineDefinition struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Nodes       []NodeConfig `json:"nodes"`
}

// IngestionContext 是单次入库执行中各节点共享的内存状态对象。
// 它记录了文档从原始二进制到最终可索引分块的全过程，包括中间产物和元数据。
//
// 数据流向（典型 fetch→parse→chunk→index 流程）：
// 1. Fetcher 节点：Source + RawBytes（从 FileStore/S3/URL 读入）
// 2. Parser 节点：RawText（经 Tika/内置解析器清洗）→ Document.Content
// 3. Chunker 节点：Chunks[]（文本分块 + embedding 向量）
// 4. Indexer 节点：调 VectorStoreService 持久化向量和元数据
//
// 关键字段说明：
// - TaskID/PipelineID：任务和流水线标识，贯穿始终用于日志追踪和数据库关联
// - Source：数据来源（file/url/s3/feishu），fetcher 节点通过这里确定如何读取原始字节
// - RawBytes：原始二进制内容，由 fetcher 写入，parser 消费
// - RawText：parser 输出，经清洗的纯文本；后续 chunker/enhancer 都依赖这里
// - Chunks：分块结果，包含 ID、Content、Vector、Metadata，indexer 负责持久化
// - EnhancedText：enhancer 产出，覆盖 RawText，供后续 chunker 使用
// - Keywords/Questions：从文本自动抽取的关键词和推荐问题（enricher 产出）
// - Metadata：文档级元数据，贯穿全程，最终写入向量库
// - Logs：节点执行日志，记录每个节点的耗时/状态/错误
// - Error：首个失败节点的错误，pipeline 快速失败时使用
type IngestionContext struct {
	TaskID           string                 `json:"taskId"`                     // 任务唯一标识
	PipelineID       string                 `json:"pipelineId"`                 // 流水线定义 ID
	Source           DocumentSource         `json:"source"`                     // 数据来源信息
	VectorSpaceID    *VectorSpaceID         `json:"vectorSpaceId,omitempty"`    // 目标向量集合
	RawBytes         []byte                 `json:"-"`                          // 原始二进制，太大不入 JSON
	DocID            string                 `json:"docId,omitempty"`            // 知识库文档 ID
	KBID             string                 `json:"kbId,omitempty"`             // 知识库 ID
	SourceURL        string                 `json:"sourceUrl,omitempty"`        // 来源 URL（用于追踪）
	FileName         string                 `json:"fileName,omitempty"`         // 文件名
	MimeType         string                 `json:"mimeType,omitempty"`         // 文件 MIME 类型
	RawText          string                 `json:"rawText,omitempty"`          // Parser 输出的纯文本
	Document         *StructuredDocument    `json:"document,omitempty"`         // 结构化文档（Content + Metadata）
	Chunks           []Chunk                `json:"chunks,omitempty"`           // 分块集合，含向量
	EnhancedText     string                 `json:"enhancedText,omitempty"`     // Enhancer 输出的优化文本
	Keywords         []string               `json:"keywords,omitempty"`         // 提取的关键词
	Questions        []string               `json:"questions,omitempty"`        // 生成的推荐问题
	Metadata         map[string]interface{} `json:"metadata,omitempty"`         // 文档级元数据
	Status           IngestionStatus        `json:"status"`                     // 当前状态（pending/running/completed/failed）
	Logs             []NodeLog              `json:"logs,omitempty"`             // 节点执行日志
	NodeLogs         []NodeLog              `json:"nodeLogs,omitempty"`         // 同 Logs 的别名
	Error            error                  `json:"-"`                          // 首个失败节点的错误
	SkipIndexerWrite bool                   `json:"skipIndexerWrite,omitempty"` // 跳过 indexer 节点（handler 自己持久化）
}

// NodeLog 记录单个节点的执行情况，由 engine.runNode 产生，持久化到任务日志表。
// 用于任务详情展示、性能分析和问题排查：哪个节点慢、哪个节点失败、输出是什么。
//
// 字段说明：
// - NodeID/NodeType/NodeOrder：节点标识和执行顺序
// - DurationMs：节点执行耗时（毫秒），性能分析用
// - Success/Error：节点是否成功及错误信息
// - Output：节点返回的摘要数据（由 NodeOutputExtractor 自动填充）
type NodeLog struct {
	NodeID     string                 `json:"nodeId"`               // 节点在 pipeline 中的唯一标识
	NodeType   string                 `json:"nodeType"`             // 节点类型（fetcher/parser/chunker 等）
	NodeOrder  int                    `json:"nodeOrder"`            // 执行顺序（1-based）
	Message    string                 `json:"message,omitempty"`    // 节点消息（成功/失败原因）
	DurationMs int64                  `json:"durationMs,omitempty"` // 执行耗时，毫秒
	Success    bool                   `json:"success"`              // 是否成功
	Error      string                 `json:"error,omitempty"`      // 错误信息（字符串化）
	Output     map[string]interface{} `json:"output,omitempty"`     // 节点输出摘要
}

// NodeConfig 是 pipeline 定义中单个节点的配置项。
// 描述节点 ID、类型、设置和执行条件，engine.Execute 按这个定义依次执行和条件判断。
//
// 字段说明：
//   - NodeID：节点在 pipeline 中的唯一标识（如 "parser"、"chunker"）
//   - NodeType：节点类型（fetcher/parser/chunker/indexer/enhancer/enricher），
//     对应注册表中的 NodeExecutor 实现
//   - Settings：节点的 JSON 配置，由具体节点 parse 和使用，如 chunker 的分块大小
//   - Condition：节点执行条件，为 JSON 布尔值/表达式/对象，由 ConditionEvaluator 求值
//   - NextNodeID：下一个节点的 ID，pipeline 按这个字段构建执行链
//
// 典型 pipeline JSON 片段：
// [
//
//	{"nodeId": "fetch", "nodeType": "fetcher", "nextNodeId": "parse"},
//	{"nodeId": "parse", "nodeType": "parser", "nextNodeId": "chunk"},
//	{"nodeId": "chunk", "nodeType": "chunker", "settings": {...}, "nextNodeId": "index"},
//	{"nodeId": "index", "nodeType": "indexer", "condition": {"field": "MimeType", "eq": "text/plain"}}
//
// ]
type NodeConfig struct {
	NodeID     string          `json:"nodeId"`               // 节点唯一标识
	NodeType   string          `json:"nodeType"`             // 节点类型
	Settings   json.RawMessage `json:"settings,omitempty"`   // 节点设置（JSON 对象）
	Condition  json.RawMessage `json:"condition,omitempty"`  // 执行条件
	NextNodeID string          `json:"nextNodeId,omitempty"` // 下一节点 ID
}

// NodeResult 单个节点执行完返回给 engine 的**统一结果**：成功/失败/跳过/终止 + 是否继续跑后续节点。
type NodeResult struct {
	Status         NodeExecutionStatus    `json:"status"`         // 细分状态：ok/skip/fail/terminate
	Success        bool                   `json:"success"`        // 是否算成功（skip 也多为 true，以便流水线继续或记日志）
	ShouldContinue bool                   `json:"shouldContinue"` // false 时 engine 在成功路径上也会 break，不再跑后面节点
	Message        string                 `json:"message,omitempty"`
	Error          error                  `json:"-"`                // 仅失败时非 nil，会写入 NodeLog
	Output         map[string]interface{} `json:"output,omitempty"` // 可选手写摘要，nil 时由 NodeOutputExtractor 补
}

func NewNodeResult(message string) NodeResult {
	// 最常见：节点成功，且要求继续执行下一节点
	return NodeResult{
		Status:         NodeExecutionStatusOK,
		Success:        true,
		ShouldContinue: true,
		Message:        message,
	}
}

func NewNodeResultWithOutput(message string, output map[string]interface{}) NodeResult {
	result := NewNodeResult(message)
	result.Output = output
	return result
}

func NewNodeResultError(err error) NodeResult {
	// 失败会中断整条 pipeline，engine 将 ingestCtx 标为 failed
	return NodeResult{
		Status:         NodeExecutionStatusFail,
		Success:        false,
		ShouldContinue: false,
		Error:          err,
		Message:        errorMessage(err),
	}
}

func NewNodeResultSkipped(reason string) NodeResult {
	// 与「条件未执行节点」不同：这是节点**内部**主动跳过，仍计成功以便日志区分
	return NodeResult{
		Status:         NodeExecutionStatusSkip,
		Success:        true,
		ShouldContinue: true,
		Message:        "Skipped: " + reason,
	}
}

func NewNodeResultTerminated(reason string) NodeResult {
	// 成功但不再往下执行（例如已在外层写完库，不需要 indexer）
	return NodeResult{
		Status:         NodeExecutionStatusTerminate,
		Success:        true,
		ShouldContinue: false,
		Message:        reason,
	}
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func normalizeEnumValue(value string) string {
	// 前端/配置里常写 "FETCHER" 或 "node-type"；统一成小写+下划线，与 JSON/DSL 更一致
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "-", "_")))
}

func (c *IngestionContext) EnsureMetadata() {
	if c.Metadata == nil {
		c.Metadata = make(map[string]interface{})
	}
}

func (c *IngestionContext) AppendLog(log NodeLog) {
	if c.Logs == nil {
		c.Logs = make([]NodeLog, 0)
	}
	c.Logs = append(c.Logs, log)
	c.NodeLogs = c.Logs // 与部分序列化/旧字段双写，避免只填其一导致丢日志
}
