package mcp

import (
	"fmt"
	"log/slog"
	"sync"
)

type Registry struct {
	mu        sync.RWMutex               // 读写锁（保护 executors）
	executors map[string]MCPToolExecutor // 工具 ID → 执行器映射
}

// NewRegistry 创建新的工具注册中心
func NewRegistry() *Registry {
	return &Registry{
		executors: make(map[string]MCPToolExecutor),
	}
}

// Register 注册工具执行器到注册中心
// 核心职责：
// 1. 从执行器获取工具定义（GetToolDefinition）
// 2. 以 ToolID 为键存储到 executors 映射
// 3. 如果同名工具已存在，直接覆盖（冲突处理策略）
// 并发安全：使用写锁保护
func (r *Registry) Register(executor MCPToolExecutor) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool := executor.GetToolDefinition()
	r.executors[executor.GetToolDefinition().ToolID] = executor
	slog.Info("MCP tool registered", "toolId", tool.ToolID, "name", tool.Name)
}

// Unregister 从注册中心移除工具执行器
func (r *Registry) Unregister(toolID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.executors, toolID)
}

// GetExecutor 根据工具 ID 获取对应的执行器
func (r *Registry) GetExecutor(toolID string) (MCPToolExecutor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	exec, ok := r.executors[toolID]
	if !ok {
		return nil, fmt.Errorf("MCP tool not found: %s", toolID)
	}
	return exec, nil
}

// ListAllTools 返回所有已注册的工具定义
func (r *Registry) ListAllTools() []MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]MCPTool, 0, len(r.executors))
	for _, exec := range r.executors {
		tools = append(tools, exec.GetToolDefinition())
	}
	return tools
}
