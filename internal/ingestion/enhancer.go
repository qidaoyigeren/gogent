package ingestion

// Enhancer 节点：**整篇** RawText 上跑 LLM（在 chunker 前）。与 enricher 成对设计：先文档级再块级，避免块级任务重复看全文导致成本爆炸。
/*
职责：整篇文档 LLM 增强（在分块前）
4种增强任务：
  - CONTEXT_ENHANCE: 上下文增强 → EnhancedText
  - KEYWORDS: 关键词提取 → ctx.Keywords[]
  - QUESTIONS: 问题生成 → ctx.Questions[]
  - METADATA: 元数据抽取 → ctx.Metadata{}

容错设计：单个任务失败不阻断流程
*/
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gogent/internal/chat"
)

// EnhanceType defines the type of enhancement task.
type EnhanceType string

const (
	EnhanceTypeContextEnhance EnhanceType = "CONTEXT_ENHANCE"
	EnhanceTypeKeywords       EnhanceType = "KEYWORDS"
	EnhanceTypeQuestions      EnhanceType = "QUESTIONS"
	EnhanceTypeMetadata       EnhanceType = "METADATA"
)

// EnhancerSettings contains settings for the enhancer node.
type EnhancerSettings struct {
	ModelID string        `json:"modelId"`
	Tasks   []EnhanceTask `json:"tasks"`
}

// EnhanceTask represents a single enhancement task.
type EnhanceTask struct {
	Type               EnhanceType `json:"type"`
	SystemPrompt       string      `json:"systemPrompt"`
	UserPromptTemplate string      `json:"userPromptTemplate"`
}

// EnhancerNode enhances text using LLM.
type EnhancerNode struct {
	llm       chat.LLMService
	evaluator *ConditionEvaluator
	promptMgr *EnhancerPromptManager
}

func NewEnhancerNode(llm chat.LLMService) *EnhancerNode {
	return &EnhancerNode{
		llm:       llm,
		evaluator: NewConditionEvaluator(),
		promptMgr: NewEnhancerPromptManager(),
	}
}

// Name returns the node name.
func (n *EnhancerNode) Name() string { return "enhancer" }

// Execute executes the enhancer node with IngestionContext.
// enhancer 是文档级 LLM 增强节点：它读取 RawText/EnhancedText，按配置执行一个或多个
// task，并把增强文本、关键词、问题或 metadata 写回 IngestionContext。
func (n *EnhancerNode) Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult {
	settings := n.parseSettings(config.Settings)
	if len(settings.Tasks) == 0 {
		return NewNodeResult("未配置增强任务")
	}

	if ingestCtx.Metadata == nil {
		ingestCtx.Metadata = make(map[string]interface{})
	}

	for _, task := range settings.Tasks {
		if task.Type == "" {
			continue
		}

		input := n.resolveInputText(ingestCtx, task.Type)
		if input == "" {
			continue
		}

		systemPrompt := task.SystemPrompt
		if systemPrompt == "" {
			// 未配置 system prompt 时使用内置任务模板，保证基础功能可用。
			systemPrompt = n.promptMgr.SystemPrompt(task.Type)
		}

		userPrompt := n.buildUserPrompt(task.UserPromptTemplate, input, ingestCtx)

		messages := []chat.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		}

		resp, err := n.chat(ctx, settings.ModelID, messages)
		if err != nil {
			// 与 enricher 不同：任务级失败不 fail 整节点，避免「只想要关键词但扩写挂」导致全文档不能入库
			slog.Warn("enhancer task failed", "type", task.Type, "err", err)
			continue
		}

		n.applyTaskResult(ingestCtx, task.Type, resp.Content) // 可能覆盖 EnhancedText/Keywords/Questions/拼 Metadata
	}

	return NewNodeResult("增强完成")
}

// chat 根据配置选择指定模型或默认模型，temperature 固定较低以降低结构化输出波动。
func (n *EnhancerNode) chat(ctx context.Context, modelID string, messages []chat.Message) (*chat.ChatResponse, error) {
	if selectable, ok := n.llm.(chat.ModelSelectableLLMService); ok && strings.TrimSpace(modelID) != "" {
		return selectable.ChatWithModelID(ctx, modelID, messages, chat.WithTemperature(0.3))
	}
	return n.llm.Chat(ctx, messages, chat.WithTemperature(0.3))
}

// parseSettings 解析 enhancer 节点配置；空配置表示不执行任何增强任务。
func (n *EnhancerNode) parseSettings(node json.RawMessage) EnhancerSettings {
	var settings EnhancerSettings
	if len(node) == 0 || string(node) == "null" {
		return settings
	}
	json.Unmarshal(node, &settings)
	return settings
}

// resolveInputText 决定某个增强任务使用哪份文本。
// CONTEXT_ENHANCE 必须基于 RawText；其他任务优先使用已增强文本，形成“先增强再抽取”的链路。
func (n *EnhancerNode) resolveInputText(ctx *IngestionContext, enhanceType EnhanceType) string {
	if enhanceType == EnhanceTypeContextEnhance {
		return ctx.RawText
	}
	if ctx.EnhancedText != "" {
		return ctx.EnhancedText
	}
	return ctx.RawText
}

// buildUserPrompt 用简单占位符把文本和上下文信息填入用户 prompt 模板。
func (n *EnhancerNode) buildUserPrompt(template, input string, ctx *IngestionContext) string {
	if template == "" {
		return input
	}
	// Simple template replacement
	result := strings.ReplaceAll(template, "${text}", input)
	result = strings.ReplaceAll(result, "${content}", input)
	result = strings.ReplaceAll(result, "${mimeType}", ctx.MimeType)
	result = strings.ReplaceAll(result, "${taskId}", ctx.TaskID)
	result = strings.ReplaceAll(result, "${pipelineId}", ctx.PipelineID)
	return result
}

// applyTaskResult 根据增强任务类型把 LLM 输出写回 context。
// 这些字段后续会进入 chunker 输入、task metadata 或 chunk/vector metadata。
func (n *EnhancerNode) applyTaskResult(ctx *IngestionContext, enhanceType EnhanceType, response string) {
	response = strings.TrimSpace(response)

	switch enhanceType {
	case EnhanceTypeContextEnhance:
		ctx.EnhancedText = response

	case EnhanceTypeKeywords:
		ctx.Keywords = n.parseStringList(response)

	case EnhanceTypeQuestions:
		ctx.Questions = n.parseStringList(response)

	case EnhanceTypeMetadata:
		meta := n.parseObject(response)
		for k, v := range meta {
			ctx.Metadata[k] = v
		}
	}
}

// parseStringList 与包内 response_parser/parseStringListResponse 同思路；此处重复是为 enhancer 包内自洽、少横向依赖（历史原因）。
func (n *EnhancerNode) parseStringList(response string) []string {
	var arr []string
	if err := json.Unmarshal([]byte(response), &arr); err == nil {
		return arr
	}

	// Try line-separated
	lines := strings.Split(response, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"'-•*`)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// parseObject 兼容 JSON object 和 key: value 文本，主要用于元数据抽取。
func (n *EnhancerNode) parseObject(response string) map[string]interface{} {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(response), &obj); err == nil {
		return obj
	}

	// Try key: value format
	result := make(map[string]interface{})
	lines := strings.Split(response, "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			result[key] = val
		}
	}
	return result
}

// EnhancerPromptManager manages prompts for enhancement tasks.
type EnhancerPromptManager struct{}

// NewEnhancerPromptManager creates a new prompt manager.
func NewEnhancerPromptManager() *EnhancerPromptManager {
	return &EnhancerPromptManager{}
}

// SystemPrompt returns the system prompt for an enhancement type.
func (m *EnhancerPromptManager) SystemPrompt(enhanceType EnhanceType) string {
	switch enhanceType {
	case EnhanceTypeContextEnhance:
		return `你是一个文本增强专家。请对以下文本进行上下文增强，添加必要的背景信息和解释，使文本更加完整和易于理解。只输出增强后的文本，不要添加其他内容。`

	case EnhanceTypeKeywords:
		return `你是一个关键词提取专家。请从以下文本中提取5-10个最重要的关键词。以JSON数组格式输出，例如：["关键词1", "关键词2", "关键词3"]`

	case EnhanceTypeQuestions:
		return `你是一个问题生成专家。请根据以下文本生成3-5个用户可能会问的问题。以JSON数组格式输出，例如：["问题1？", "问题2？", "问题3？"]`

	case EnhanceTypeMetadata:
		return `你是一个元数据提取专家。请从以下文本中提取关键元数据信息，如：作者、日期、主题、来源等。以JSON对象格式输出，例如：{"author": "张三", "topic": "人工智能"}`

	default:
		return ""
	}
}

// Process implements the old interface for backward compatibility.
func (n *EnhancerNode) Process(ctx context.Context, doc *Document) error {
	// legacy Document 只回填 metadata，不改变旧 pipeline 的字段契约。
	ingestCtx := &IngestionContext{
		DocID:    doc.ID,
		FileName: doc.FileName,
		RawText:  doc.Parsed,
		Metadata: make(map[string]interface{}),
	}

	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: "enhancer"})
	if !result.Success {
		if result.Error != nil {
			return result.Error
		}
		return errors.New(result.Message)
	}

	// Apply results back to document
	doc.Metadata = make(map[string]string)
	for k, v := range ingestCtx.Metadata {
		doc.Metadata[k] = fmt.Sprintf("%v", v)
	}

	return nil
}
