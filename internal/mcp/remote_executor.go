package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RemoteExecutor 远程 MCP 工具执行器实现
type RemoteExecutor struct {
	client  Client  // MCP 客户端
	toolDef MCPTool // 工具定义
}

// NewRemoteExecutor 创建新的远程工具执行器
func NewRemoteExecutor(client Client, toolDef MCPTool) *RemoteExecutor {
	return &RemoteExecutor{
		client:  client,
		toolDef: toolDef,
	}
}

// GetToolDefinition 返回工具定义
func (e *RemoteExecutor) GetToolDefinition() MCPTool {
	return e.toolDef
}

// Execute 执行远程工具调用
// 工作流程：
// 1. 记录开始时间
// 2. 调用 Client.CallTool
// 3. 计算耗时
// 4. 处理失败情况（返回错误响应）
// 5. 处理空结果（返回错误响应）
// 6. 返回成功响应
//
// 注意：
// - 失败时返回 Success=false，不返回 error（保证调用链不中断）
// - 记录耗时用于性能监控
func (e *RemoteExecutor) Execute(ctx context.Context, req MCPRequest) (*MCPResponse, error) {
	start := time.Now()

	toolOut, err := e.client.CallTool(ctx, e.toolDef.ToolID, req.Parameters)
	costMs := time.Since(start).Milliseconds()

	if err != nil {
		reason := err.Error()
		slog.Warn("Remote MCP tool call failed",
			"toolId", req.ToolID,
			"reason", reason,
			"costMs", costMs)

		// 失败时返回错误响应（不返回 error，保证调用链不中断）
		return &MCPResponse{
			Success:   false,
			ToolID:    req.ToolID,
			ErrorCode: "REMOTE_CALL_FAILED",
			ErrorMsg:  fmt.Sprintf("远程工具调用失败: %s", reason),
			CostMs:    costMs,
		}, nil
	}

	if toolOut.Text == "" {
		slog.Warn("Remote MCP tool returned empty result",
			"toolId", req.ToolID,
			"costMs", costMs)

		// 空结果也视为失败
		return &MCPResponse{
			Success:   false,
			ToolID:    req.ToolID,
			ErrorCode: "EMPTY_RESULT",
			ErrorMsg:  "远程工具返回空结果",
			CostMs:    costMs,
		}, nil
	}

	slog.Debug("Remote MCP tool call succeeded",
		"toolId", req.ToolID,
		"resultLength", len(toolOut.Text),
		"costMs", costMs)

	resp := &MCPResponse{
		Success:    true,
		ToolID:     req.ToolID,
		TextResult: toolOut.Text,
		CostMs:     costMs,
	}
	if len(toolOut.Data) > 0 {
		resp.Data = toolOut.Data
	}
	return resp, nil
}
