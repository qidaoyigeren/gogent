package mcp

import "encoding/json"

type MCPTool struct {
	ToolID      string          `json:"toolId"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema
	RequireUser bool            `json:"requireUserId"`
}

// MCPRequest is the request to execute an MCP tool.
type MCPRequest struct {
	ToolID     string                 `json:"toolId"`
	UserID     string                 `json:"userId,omitempty"`
	Parameters map[string]interface{} `json:"parameters"`
}

// MCPResponse is the response from an MCP tool execution.
type MCPResponse struct {
	Success    bool        `json:"success"`
	ToolID     string      `json:"toolId"`
	TextResult string      `json:"textResult,omitempty"`
	Data       interface{} `json:"data,omitempty"`
	ErrorCode  string      `json:"errorCode,omitempty"`
	ErrorMsg   string      `json:"errorMsg,omitempty"`
	CostMs     int64       `json:"costMs,omitempty"`
}
