package mcp

import "encoding/json"

type MCPTool struct {
	ToolID      string          `json:"toolId"`               // 工具唯一 ID（通常等于 Name）
	Name        string          `json:"name"`                 // 工具名称
	Description string          `json:"description"`          // 工具描述（LLM 用于理解工具用途）
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema（参数定义）
	RequireUser bool            `json:"requireUserId"`        // 是否需要用户 ID
}

// MCPRequest MCP 工具执行请求
// 核心职责：
// 1. 携带工具 ID、用户 ID、参数
// 2. 由 Executor 执行时传入
type MCPRequest struct {
	ToolID     string                 `json:"toolId"`           // 工具 ID
	UserID     string                 `json:"userId,omitempty"` // 用户 ID（可选）
	Parameters map[string]interface{} `json:"parameters"`       // 工具参数（LLM 提取）
}

// MCPResponse MCP 工具执行响应
// 核心职责：
// 1. 返回执行结果（Success/TextResult/Data）
// 2. 错误信息（ErrorCode/ErrorMsg）
// 3. 性能指标（CostMs）
type MCPResponse struct {
	Success    bool        `json:"success"`              // 是否成功
	ToolID     string      `json:"toolId"`               // 工具 ID
	TextResult string      `json:"textResult,omitempty"` // 文本结果（主要返回格式）
	Data       interface{} `json:"data,omitempty"`       // 结构化数据（JSON 对象）
	ErrorCode  string      `json:"errorCode,omitempty"`  // 错误码（失败时）
	ErrorMsg   string      `json:"errorMsg,omitempty"`   // 错误信息（失败时）
	CostMs     int64       `json:"costMs,omitempty"`     // 执行耗时（毫秒）
}
