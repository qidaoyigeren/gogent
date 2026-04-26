package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/chat"
	"gogent/pkg/llmutil"
	"log/slog"
	"strings"
)

// ParamExtractor MCP 工具参数提取器
type ParamExtractor struct {
	llm chat.LLMService // LLM 服务
}

func NewParamExtractor(llm chat.LLMService) *ParamExtractor {
	return &ParamExtractor{llm: llm}
}

const paramExtractionPrompt = `你是一个参数提取助手。根据用户的问题和工具定义，提取调用工具所需的参数。

工具定义：
- 工具 ID: %s
- 描述: %s
- 参数 Schema: %s

附加提取要求：
%s

用户问题：%s

请以 JSON 对象格式返回参数，键为参数名，值为从用户问题中提取的值。
如果某个参数无法从问题中提取，使用 null。
只返回 JSON 对象，不要其他文字。`

// Extract 使用 LLM 从用户问题中提取工具参数
// 工作流程：
// 1. 构建提取提示词（包含工具定义和 Schema）
// 2. 调用 LLM（temperature=0.1，保证稳定性）
// 3. 提取思考内容并清理 JSON 块
// 4. 解析 JSON 参数
// 5. 移除 null 值
// 6. 根据 Schema 过滤和填充默认值
func (e *ParamExtractor) Extract(ctx context.Context, tool MCPTool, query string, customPrompt string) (map[string]interface{}, error) {
	schemaStr := "{}"
	if tool.Parameters != nil {
		schemaStr = string(tool.Parameters)
	}

	extractionHint := strings.TrimSpace(customPrompt)
	if extractionHint == "" {
		extractionHint = "无"
	}
	prompt := fmt.Sprintf(paramExtractionPrompt, tool.ToolID, tool.Description, schemaStr, extractionHint, query)

	resp, err := e.llm.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, chat.WithTemperature(0.1))

	if err != nil {
		return nil, fmt.Errorf("param extraction LLM call failed: %w", err)
	}

	content := llmutil.ExtractThinkContent(resp.Content)
	content = llmutil.CleanJSONBlock(content)

	var params map[string]interface{}
	if err := json.Unmarshal([]byte(content), &params); err != nil {
		slog.Warn("failed to parse extracted params", "err", err, "raw", content)
		params = make(map[string]interface{})
	}

	// Remove null values
	for k, v := range params {
		if v == nil {
			delete(params, k)
		}
	}

	// 对齐 Java 行为：
	// - 只保留 Schema 中声明的参数
	// - 填补缺失的默认值
	if tool.Parameters != nil && len(tool.Parameters) > 0 {
		allowed, defaults := parseJSONSchema(tool.Parameters)
		if len(allowed) > 0 {
			// 移除未在 Schema 中声明的参数
			for k := range params {
				if !allowed[k] {
					delete(params, k)
				}
			}
			// 填充默认值
			for k, v := range defaults {
				if _, ok := params[k]; !ok && v != nil {
					params[k] = v
				}
			}
		}
	}

	return params, nil
}

// parseJSONSchema 从 JSON Schema 中提取允许的参数名和默认值
// 尽力解析：如果 Schema 格式不匹配预期，返回空集合
// 返回值：
// - allowed: 允许的参数名集合
// - defaults: 参数默认值集合
func parseJSONSchema(schemaBytes []byte) (map[string]bool, map[string]interface{}) {
	type prop struct {
		Default interface{} `json:"default"`
	}
	type schema struct {
		Properties map[string]prop `json:"properties"`
	}

	var s schema
	if err := json.Unmarshal(schemaBytes, &s); err != nil || len(s.Properties) == 0 {
		return nil, nil
	}

	allowed := make(map[string]bool, len(s.Properties))
	defaults := make(map[string]interface{}, len(s.Properties))
	for name, p := range s.Properties {
		allowed[name] = true
		if p.Default != nil {
			defaults[name] = p.Default
		}
	}
	return allowed, defaults
}

// SelectTool 使用 LLM 选择最适合用户问题的工具
// 工作流程：
// 1. 如果只有 0 或 1 个工具，直接返回
// 2. 构建工具列表提示词
// 3. 调用 LLM 选择工具
// 4. 返回 toolId
//
// 参数：
// - tools: 候选工具列表
// - query: 用户问题
//
// 返回值：
// - 成功：返回选择的 toolId
// - 失败：返回错误
func (e *ParamExtractor) SelectTool(ctx context.Context, tools []MCPTool, query string) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	if len(tools) == 1 {
		return tools[0].ToolID, nil
	}

	var sb strings.Builder
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("- toolId: %s, name: %s, description: %s\n", t.ToolID, t.Name, t.Description))
	}

	prompt := fmt.Sprintf(`从以下工具中选择最适合回答用户问题的工具：

工具列表：
%s

用户问题：%s

只返回工具的toolId，不要其他文字。`, sb.String(), query)

	resp, err := e.llm.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, chat.WithTemperature(0.1))

	if err != nil {
		return "", err
	}

	toolID := strings.TrimSpace(llmutil.ExtractThinkContent(resp.Content))
	return toolID, nil
}
