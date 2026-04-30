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

// ParseIngestionNodeType 将输入的字符串值解析为 IngestionNodeType 类型
func ParseIngestionNodeType(value string) (IngestionNodeType, error) {
	// 使用 normalizeEnumValue 对输入值进行标准化处理
	switch normalizeEnumValue(value) {
	case string(IngestionNodeTypeFetcher): // 匹配获取器类型
		return IngestionNodeTypeFetcher, nil
	case string(IngestionNodeTypeParser): // 匹配解析器类型
		return IngestionNodeTypeParser, nil
	case string(IngestionNodeTypeEnhancer): // 匹配增强器类型
		return IngestionNodeTypeEnhancer, nil
	case string(IngestionNodeTypeChunker): // 匹配分块器类型
		return IngestionNodeTypeChunker, nil
	case string(IngestionNodeTypeEnricher): // 匹配丰富器类型
		return IngestionNodeTypeEnricher, nil
	case string(IngestionNodeTypeIndexer): // 匹配索引器类型
		return IngestionNodeTypeIndexer, nil
	default: // 处理未知类型的情况
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

type IngestionContext struct {
	TaskID           string                 `json:"taskId"`
	PipelineID       string                 `json:"pipelineId"`
	Source           DocumentSource         `json:"source"`
	VectorSpaceID    *VectorSpaceID         `json:"vectorSpaceId,omitempty"`
	RawBytes         []byte                 `json:"-"`
	DocID            string                 `json:"docId,omitempty"`
	KBID             string                 `json:"kbId,omitempty"`
	SourceURL        string                 `json:"sourceUrl,omitempty"`
	FileName         string                 `json:"fileName,omitempty"`
	MimeType         string                 `json:"mimeType,omitempty"`
	RawText          string                 `json:"rawText,omitempty"`
	Document         *StructuredDocument    `json:"document,omitempty"`
	Chunks           []Chunk                `json:"chunks,omitempty"`
	EnhancedText     string                 `json:"enhancedText,omitempty"`
	Keywords         []string               `json:"keywords,omitempty"`
	Questions        []string               `json:"questions,omitempty"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"`
	Status           IngestionStatus        `json:"status"`
	Logs             []NodeLog              `json:"logs,omitempty"`
	NodeLogs         []NodeLog              `json:"nodeLogs,omitempty"`
	Error            error                  `json:"-"`
	SkipIndexerWrite bool                   `json:"skipIndexerWrite,omitempty"`
}

type NodeLog struct {
	NodeID     string                 `json:"nodeId"`
	NodeType   string                 `json:"nodeType"`
	NodeOrder  int                    `json:"nodeOrder,omitempty"`
	Message    string                 `json:"message,omitempty"`
	DurationMs int64                  `json:"durationMs,omitempty"`
	Success    bool                   `json:"success"`
	Error      string                 `json:"error,omitempty"`
	Output     map[string]interface{} `json:"output,omitempty"`
}

type NodeConfig struct {
	NodeID     string          `json:"nodeId"`
	NodeType   string          `json:"nodeType"`
	Settings   json.RawMessage `json:"settings,omitempty"`
	Condition  json.RawMessage `json:"condition,omitempty"`
	NextNodeID string          `json:"nextNodeId,omitempty"`
}

type NodeResult struct {
	Status         NodeExecutionStatus    `json:"status"`
	Success        bool                   `json:"success"`
	ShouldContinue bool                   `json:"shouldContinue"`
	Message        string                 `json:"message,omitempty"`
	Error          error                  `json:"-"`
	Output         map[string]interface{} `json:"output,omitempty"`
}

func NewNodeResult(message string) NodeResult {
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
	return NodeResult{
		Status:         NodeExecutionStatusFail,
		Success:        false,
		ShouldContinue: false,
		Error:          err,
		Message:        errorMessage(err),
	}
}

func NewNodeResultSkipped(reason string) NodeResult {
	return NodeResult{
		Status:         NodeExecutionStatusSkip,
		Success:        true,
		ShouldContinue: true,
		Message:        "Skipped: " + reason,
	}
}

func NewNodeResultTerminated(reason string) NodeResult {
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
	c.NodeLogs = c.Logs
}
