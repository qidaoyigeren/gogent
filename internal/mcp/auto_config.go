package mcp

import (
	"context"
	"gogent/internal/config"
	"log/slog"
)

// AutoConfigure 自动配置 MCP 工具
// 核心职责：
// 1. 遍历配置中的所有 MCP Server
// 2. 连接并初始化每个 Server
// 3. 获取工具列表并注册到 Registry
func AutoConfigure(ctx context.Context, registry *Registry, mcpConfig config.MCPConfig) {
	if len(mcpConfig.Servers) == 0 {
		slog.Info("no MCP servers configured, skipping")
		return
	}

	for _, serverCfg := range mcpConfig.Servers {
		if serverCfg.URL == "" {
			slog.Warn("MCP server URL is empty, skipping", "name", serverCfg.Name)
			continue
		}

		slog.Info("connecting to MCP server", "name", serverCfg.Name, "url", serverCfg.URL)

		// 创建 HTTP 客户端
		client := NewHTTPClient(serverCfg.URL)

		// 初始化连接
		if !client.Initialize(ctx) {
			slog.Error("MCP server initialization failed, skipping tool registration",
				"name", serverCfg.Name,
				"url", serverCfg.URL)
			continue
		}

		// 获取可用工具列表
		tools, err := client.ListTools(ctx)
		if err != nil {
			slog.Error("failed to list MCP tools",
				"name", serverCfg.Name,
				"url", serverCfg.URL,
				"err", err)
			continue
		}

		if len(tools) == 0 {
			slog.Info("no MCP tools found on server, skipping registration",
				"name", serverCfg.Name)
			continue
		}

		slog.Info("MCP server returned tools",
			"name", serverCfg.Name,
			"count", len(tools))

		// 注册每个工具（使用 RemoteExecutor）
		for _, tool := range tools {
			executor := NewRemoteExecutor(client, tool)
			registry.Register(executor)
			slog.Info("registered remote MCP tool",
				"toolId", tool.ToolID,
				"name", tool.Name,
				"server", serverCfg.Name)
		}
	}
}
