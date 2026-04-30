package main

import (
	"fmt"
	"gogent/internal/mcpserver"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	// 获取端口（从环境变量或默认值）
	port := os.Getenv("MCP_SERVER_PORT")
	if port == "" {
		port = "9099"
	}

	// 创建 HTTP 路由器
	mux := http.NewServeMux()

	// 创建 MCP Server（自动注册内置工具）
	srv := mcpserver.NewServer()
	srv.RegisterRoutes(mux)

	// 启动 HTTP 服务
	addr := fmt.Sprintf(":%s", port)
	slog.Info("go mcp server starting", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("go mcp server exited", "err", err)
		os.Exit(1)
	}
}
