package mcp

import "context"

// MCPToolExecutor MCP 工具执行器接口
// 1. GetToolDefinition: 返回工具定义（用于注册和展示）
// 2. Execute: 执行工具调用
type MCPToolExecutor interface {
	GetToolDefinition() MCPTool
	Execute(ctx context.Context, req MCPRequest) (*MCPResponse, error)
}
