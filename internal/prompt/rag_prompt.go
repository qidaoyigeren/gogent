package prompt

import (
	"fmt"
	"gogent/internal/chat"
	"gogent/internal/mcp"
	"gogent/internal/retrieve"
	"strings"
)

// ScenarioType 提示词场景类型
// 核心职责：根据检索结果选择不同的提示模板
type ScenarioType int

const (
	ScenarioKBOnly  ScenarioType = iota // 仅知识库检索结果
	ScenarioMCPOnly                     // 仅 MCP 工具结果
	ScenarioMixed                       // 混合（知识库 + MCP 工具）
)

// RAGPromptService RAG 提示词构建服务
type RAGPromptService struct {
	loader *templateLoader
}

func NewRAGPromptService() *RAGPromptService {
	return &RAGPromptService{loader: newTemplateLoader()}
}

// BuildPrompt 构建最终的 LLM 提示词消息列表
// 核心职责：
// 1. 检测场景（KB/MCP/Mixed）
// 2. 选择并填充模板
// 3. 注入对话摘要（如果有）
// 4. 添加证据上下文（MCP 结果 + 知识库文档）
// 5. 添加对话历史
// 6. 添加当前查询（多子问题时编号）
func (s *RAGPromptService) BuildPrompt(
	query string,
	subQuestions []string,
	summary string,
	chunks []retrieve.DocumentChunk,
	mcpResults []*mcp.MCPResponse,
	intentTemplate string,
	history []chat.Message,
) []chat.Message {
	// 1. 检测场景（根据是否有 KB chunks 和 MCP results）
	scenario := s.detectScenario(chunks, mcpResults)
	// 2. 构建系统提示词
	var systemPrompt string
	if strings.TrimSpace(intentTemplate) != "" {
		systemPrompt = fillSlots(intentTemplate, map[string]string{
			"kb_context":  "",
			"mcp_context": "",
			"summary":     summary,
		})
	} else {
		switch scenario {
		case ScenarioKBOnly:
			systemPrompt = s.buildKBOnlyPrompt()
		case ScenarioMCPOnly:
			systemPrompt = s.buildMCPOnlyPrompt()
		case ScenarioMixed:
			systemPrompt = s.buildMixedPrompt()
		}
	}
	// 3. 注入对话摘要（前缀）
	if summary != "" {
		systemPrompt = fmt.Sprintf("对话摘要：%s\n\n%s", summary, systemPrompt)
	}

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt}, // 系统提示词
	}
	// 4. 添加 MCP 工具结果（system 消息）
	if mcpContext := formatMCPContext(mcpResults); mcpContext != "" {
		messages = append(messages, chat.Message{
			Role:    "system",
			Content: formatEvidence("## 动态数据片段", mcpContext),
		})
	}
	// 5. 添加知识库文档（user 消息）
	if kbContext := formatChunksBySubQuestion(chunks); kbContext != "" {
		messages = append(messages, chat.Message{
			Role:    "user",
			Content: formatEvidence("## 文档内容", kbContext),
		})
	}
	// 6. 添加对话历史
	messages = append(messages, history...)
	// 7. 添加当前查询（多子问题时编号）
	messages = append(messages, chat.Message{Role: "user", Content: buildUserMessage(query, subQuestions)})

	return messages
}

// detectScenario 检测提示词场景
func (s *RAGPromptService) detectScenario(chunks []retrieve.DocumentChunk, mcpResults []*mcp.MCPResponse) ScenarioType {
	hasKB := len(chunks) > 0
	hasMCP := false
	for _, result := range mcpResults {
		if result != nil && result.Success {
			hasMCP = true
			break
		}
	}
	if hasKB && hasMCP {
		return ScenarioMixed
	} else if hasKB {
		return ScenarioKBOnly
	}
	return ScenarioMCPOnly
}

const (
	kbTemplateFallback    = "知识文档：\n{{kb_context}}"                             // KB 场景降级模板
	mcpTemplateFallback   = "工具调用结果：\n{{mcp_context}}"                          // MCP 场景降级模板
	mixedTemplateFallback = "知识文档：\n{{kb_context}}\n\n工具调用结果：\n{{mcp_context}}" // Mixed 场景降级模板
)

func (s *RAGPromptService) buildKBOnlyPrompt() string {
	tpl := s.loader.load("rag-kb.txt", kbTemplateFallback)
	return fillSlots(tpl, map[string]string{"kb_content": ""})
}

func (s *RAGPromptService) buildMCPOnlyPrompt() string {
	tpl := s.loader.load("rag-mcp.txt", mcpTemplateFallback)
	return fillSlots(tpl, map[string]string{"mcp_content": ""})
}

func (s *RAGPromptService) buildMixedPrompt() string {
	tpl := s.loader.load("rag-mixed.txt", mixedTemplateFallback)
	return fillSlots(tpl, map[string]string{"kb_content": "", "mcp_content": ""})
}

// formatMCPContext 格式化 MCP 工具结果
// 输出格式：
// [工具: tool-id] 结果: xxx
// [工具: tool-id] 调用失败: error
func formatMCPContext(mcpResults []*mcp.MCPResponse) string {
	var sb strings.Builder
	for _, r := range mcpResults {
		if r == nil {
			continue
		}
		if r.Success {
			sb.WriteString(fmt.Sprintf("[工具: %s] 结果: %s\n", r.ToolID, r.TextResult))
		} else {
			sb.WriteString(fmt.Sprintf("[工具: %s] 调用失败: %s\n", r.ToolID, r.ErrorMsg))
		}
	}
	return strings.TrimSpace(sb.String())
}

// formatEvidence 格式化证据块（带标题）
// 示例：formatEvidence("## 文档内容", "文档 A\n文档 B") → "## 文档内容\n\n文档 A\n文档 B"
func formatEvidence(header, body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	return fmt.Sprintf("%s\n\n%s", header, body)
}

// formatChunksBySubQuestion 按子问题分组知识库文档
// 输出格式：
// ---
// **子问题**：xxx
//
// **相关文档**：
// [文档 1] (来源: kb-123, 置信度: 0.85)
// 文档内容...
func formatChunksBySubQuestion(chunks []retrieve.DocumentChunk) string {
	if len(chunks) == 0 {
		return ""
	}
	groups := make(map[string][]retrieve.DocumentChunk)
	order := make([]string, 0)
	for _, c := range chunks {
		q := ""
		if c.Metadata != nil {
			q = strings.TrimSpace(c.Metadata["subQuestion"])
		}
		if q == "" {
			q = "主问题"
		}
		if _, ok := groups[q]; !ok {
			order = append(order, q)
		}
		groups[q] = append(groups[q], c)
	}

	var sb strings.Builder
	docNo := 1
	for _, q := range order {
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("**子问题**：%s\n\n", q))
		sb.WriteString("**相关文档**：\n")
		for _, c := range groups[q] {
			sb.WriteString(fmt.Sprintf("[文档%d] (来源: %s, 置信度: %.2f)\n%s\n\n", docNo, c.KBID, c.Score, c.Content))
			docNo++
		}
	}
	return strings.TrimSpace(sb.String())
}

// buildUserMessage 构建用户消息
// 逻辑：
// 1. 如果子问题数量 ≤ 1，直接返回查询
// 2. 如果子问题数量 > 1，使用编号格式（降低漏答风险）
// 示例：
// "请基于上述文档内容，回答以下问题：
//  1. 子问题 1
//  2. 子问题 2"
func buildUserMessage(query string, subQuestions []string) string {
	if len(subQuestions) <= 1 {
		return query
	}
	var sb strings.Builder
	sb.WriteString("请基于上述文档内容，回答以下问题：\n\n")
	for i, q := range subQuestions {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, q))
	}
	msg := strings.TrimSpace(sb.String())
	if msg == "" {
		return query
	}
	return msg
}
