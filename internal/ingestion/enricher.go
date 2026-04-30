package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/chat"
	"strings"
)

/*
// 职责：对每个 chunk 单独 LLM 增强（在分块后）
3种块增强任务：
  - keywords: chunk关键词 → chunk.Metadata["keywords"]
  - summary: chunk摘要 → chunk.Metadata["summary"]
  - metadata: 结构化元数据 → chunk.Metadata{}

与 Enhancer 的区别：
  - Enhancer: 文档级，分块前，影响后续所有chunk
  - Enricher: Chunk级，分块后，每个chunk独立增强

文档元数据下沉：
  - attachDocumentMetadata: 把文档级Metadata附加到每个chunk

复杂度 O(块数×任务数) 次 Chat；大文档成本高，适合关键词/摘要等短输出。
*/

type ChunkEnrichType string

const (
	ChunkEnrichTypeKeywords ChunkEnrichType = "keywords"
	ChunkEnrichTypeSummary  ChunkEnrichType = "summary"
	ChunkEnrichTypeMetadata ChunkEnrichType = "metadata"
)

type EnricherSettings struct {
	ModelID                string            `json:"modelId"`
	AttachDocumentMetadata *bool             `json:"attachDocumentMetadata"`
	Tasks                  []ChunkEnrichTask `json:"tasks"`
}

type ChunkEnrichTask struct {
	Type               ChunkEnrichType `json:"type"`
	SystemPrompt       string          `json:"systemPrompt"`
	UserPromptTemplate string          `json:"userPromptTemplate"`
}

type EnricherNode struct {
	llm       chat.LLMService
	promptMgr *EnricherPromptManager
}

// NewEnricherNode 创建 chunk 级增强节点。
func NewEnricherNode(llm chat.LLMService) *EnricherNode {
	return &EnricherNode{
		llm:       llm,
		promptMgr: NewEnricherPromptManager(),
	}
}

func (n *EnricherNode) Name() string { return "enricher" }

// Execute 对每个 chunk 调用 LLM，补充 chunk 级关键词、摘要或元数据。
// 它和 enhancer 的区别是：enhancer 作用于整篇文档，enricher 作用于已经切好的每个块。
func (n *EnricherNode) Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult {
	ingestCtx.EnsureMetadata()
	if len(ingestCtx.Chunks) == 0 {
		return NewNodeResult("No chunks to enrich")
	}

	settings := n.parseSettings(config.Settings)
	if len(settings.Tasks) == 0 {
		return NewNodeResult("No enricher tasks configured")
	}
	if n.llm == nil {
		return NewNodeResultError(fmt.Errorf("enricher requires llm service"))
	}

	attachMetadata := settings.AttachDocumentMetadata == nil || *settings.AttachDocumentMetadata // 默认真：块级也带 doc 级键，方便向量过滤 kb_id 等
	for i := range ingestCtx.Chunks {
		if strings.TrimSpace(ingestCtx.Chunks[i].Content) == "" {
			// 空 chunk 无法提供有效上下文，直接跳过。
			continue
		}
		if ingestCtx.Chunks[i].Metadata == nil {
			ingestCtx.Chunks[i].Metadata = map[string]string{}
		}
		if attachMetadata {
			// 默认把文档级 metadata 下沉到 chunk，方便向量检索时按文档属性过滤。
			for k, v := range ingestCtx.Metadata {
				ingestCtx.Chunks[i].Metadata[k] = fmt.Sprintf("%v", v)
			}
		}

		for _, task := range settings.Tasks {
			if task.Type == "" {
				continue
			}
			systemPrompt := task.SystemPrompt
			if systemPrompt == "" {
				systemPrompt = n.promptMgr.SystemPrompt(task.Type)
			}
			userPrompt := n.buildUserPrompt(task.UserPromptTemplate, &ingestCtx.Chunks[i], ingestCtx)
			resp, err := n.chat(ctx, settings.ModelID, []chat.Message{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: userPrompt},
			})
			if err != nil {
				// 任一块任一任务失败即整节点失败：与 enhancer 宽松策略对偶，因块级元数据与入库一致性绑定更强
				return NewNodeResultError(err)
			}
			n.applyResult(&ingestCtx.Chunks[i], task.Type, resp.Content) // 同一 chunk 上多 task 会覆盖同名字段
		}
	}

	return NewNodeResultWithOutput("Enricher completed", map[string]interface{}{
		"chunkCount": len(ingestCtx.Chunks),
	})
}

// chat 根据 modelID 选择指定模型，否则走默认 LLM 服务。
func (n *EnricherNode) chat(ctx context.Context, modelID string, messages []chat.Message) (*chat.ChatResponse, error) {
	if selectable, ok := n.llm.(chat.ModelSelectableLLMService); ok && strings.TrimSpace(modelID) != "" {
		return selectable.ChatWithModelID(ctx, modelID, messages, chat.WithTemperature(0.3))
	}
	return n.llm.Chat(ctx, messages, chat.WithTemperature(0.3))
}

// Process 适配 legacy Document pipeline，复用新的 Execute 后把 chunks 写回旧结构。
func (n *EnricherNode) Process(ctx context.Context, doc *Document) error {
	metadata := map[string]interface{}{}
	for k, v := range doc.Metadata {
		metadata[k] = v
	}
	ingestCtx := &IngestionContext{
		DocID:    doc.ID,
		KBID:     doc.KBID,
		FileName: doc.FileName,
		Metadata: metadata,
		Chunks:   doc.Chunks,
	}
	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: string(IngestionNodeTypeEnricher)})
	if !result.Success {
		return result.Error
	}
	doc.Chunks = ingestCtx.Chunks
	return nil
}

// parseSettings 解析 enricher 节点配置；空配置表示没有 chunk 级增强任务。
func (n *EnricherNode) parseSettings(raw json.RawMessage) EnricherSettings {
	if len(raw) == 0 || string(raw) == "null" {
		return EnricherSettings{}
	}
	var settings EnricherSettings
	_ = json.Unmarshal(raw, &settings)
	return settings
}

// buildUserPrompt 将 chunk 内容和上下文字段填入用户 prompt 模板。
func (n *EnricherNode) buildUserPrompt(template string, chunk *Chunk, ctx *IngestionContext) string {
	input := chunk.Content
	if strings.TrimSpace(template) == "" {
		return input
	}
	chunkIndex := ""
	if chunk.Metadata != nil {
		chunkIndex = chunk.Metadata["chunkIndex"]
	}
	result := strings.ReplaceAll(template, "${text}", input)
	result = strings.ReplaceAll(result, "${content}", input)
	result = strings.ReplaceAll(result, "${chunkIndex}", chunkIndex)
	result = strings.ReplaceAll(result, "${taskId}", ctx.TaskID)
	result = strings.ReplaceAll(result, "${pipelineId}", ctx.PipelineID)
	return result
}

// applyResult 把 LLM 返回写入 chunk.Metadata。
// keywords 使用逗号拼接，summary 直接保存文本，metadata 则逐项展开到 metadata map。
func (n *EnricherNode) applyResult(chunk *Chunk, enrichType ChunkEnrichType, response string) {
	if chunk.Metadata == nil {
		chunk.Metadata = map[string]string{}
	}
	switch enrichType {
	case ChunkEnrichTypeKeywords:
		chunk.Metadata["keywords"] = strings.Join(parseStringListResponse(response), ",")
	case ChunkEnrichTypeSummary:
		chunk.Metadata["summary"] = strings.TrimSpace(response)
	case ChunkEnrichTypeMetadata:
		for k, v := range parseObjectResponse(response) {
			chunk.Metadata[k] = fmt.Sprintf("%v", v)
		}
	}
}

type EnricherPromptManager struct{}

func NewEnricherPromptManager() *EnricherPromptManager { return &EnricherPromptManager{} }

// SystemPrompt 返回每类 chunk 增强任务的默认系统提示词。
func (m *EnricherPromptManager) SystemPrompt(enrichType ChunkEnrichType) string {
	switch enrichType {
	case ChunkEnrichTypeKeywords:
		return `你是一个关键词提取专家。请从以下文本分块中提取关键词，以JSON数组格式输出。`
	case ChunkEnrichTypeSummary:
		return `你是一个摘要专家。请对以下文本分块生成简洁摘要，只输出摘要。`
	case ChunkEnrichTypeMetadata:
		return `你是一个元数据提取专家。请从以下文本分块中提取结构化元数据，并以JSON对象格式输出。`
	default:
		return ""
	}
}
