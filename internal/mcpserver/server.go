package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
)

type JSONRPCRequest struct {
	JSONRPC string                 `json:"jsonrpc"`          // 协议版本（固定 "2.0"）
	ID      interface{}            `json:"id,omitempty"`     // 请求 ID（通知为 nil）
	Method  string                 `json:"method"`           // 方法名
	Params  map[string]interface{} `json:"params,omitempty"` // 参数
}

// JSONRPCResponse JSON-RPC 2.0 响应结构体
type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`          // 协议版本
	ID      interface{} `json:"id,omitempty"`     // 请求 ID（与请求对应）
	Result  interface{} `json:"result,omitempty"` // 结果（成功时）
	Error   *RPCError   `json:"error,omitempty"`  // 错误（失败时）
}

// RPCError JSON-RPC 错误结构体
type RPCError struct {
	Code    int    `json:"code"`    // 错误码
	Message string `json:"message"` // 错误信息
}

// ToolDefinition MCP 工具定义
type ToolDefinition struct {
	Name        string                                             `json:"name"`        // 工具名称
	Description string                                             `json:"description"` // 工具描述
	InputSchema map[string]interface{}                             `json:"inputSchema"` // 参数 Schema
	Handler     func(map[string]interface{}) (string, bool, error) // 处理函数
}

// Server MCP 服务器
type Server struct {
	tools map[string]ToolDefinition // 工具注册表
}

// NewServer 创建新的 MCP 服务器
// 自动注册内置工具（BuiltinTools）
func NewServer() *Server {
	s := &Server{tools: make(map[string]ToolDefinition)}
	for _, tool := range BuiltinTools() {
		s.tools[tool.Name] = tool
	}
	return s
}

// RegisterRoutes 注册 HTTP 路由
// 路由：
// - /mcp: MCP JSON-RPC 端点
// - /health: 健康检查
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// handleMCP 处理 MCP JSON-RPC 请求
// 工作流程：
// 1. 检查 HTTP 方法（只允许 POST）
// 2. 解析 JSON-RPC 请求
// 3. 如果是通知（ID 为 nil），返回 204
// 4. 否则分发请求并返回响应
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   &RPCError{Code: -32700, Message: "Parse error"},
		})
		return
	}

	// JSON-RPC 通知没有 id，不需要响应体
	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp := s.dispatch(&req)
	writeJSON(w, http.StatusOK, resp)
}

// dispatch 分发 JSON-RPC 请求到对应处理方法
// 支持的方法：
// - initialize: 初始化连接
// - tools/list: 获取工具列表
// - tools/call: 调用工具
func (s *Server) dispatch(req *JSONRPCRequest) JSONRPCResponse {
	switch req.Method {
	case "initialize":
		return JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2026-02-28",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{"listChanged": false},
				},
				"serverInfo": map[string]interface{}{
					"name":    "go-ragent-mcp-server",
					"version": "0.0.1",
				},
			},
		}
	case "tools/list":
		tools := make([]map[string]interface{}, 0, len(s.tools))
		for _, tool := range s.tools {
			tools = append(tools, map[string]interface{}{
				"name":        tool.Name,
				"description": tool.Description,
				"inputSchema": tool.InputSchema,
			})
		}
		return JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": tools,
			},
		}
	case "tools/call":
		name, _ := req.Params["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return rpcErr(req.ID, -32602, "Missing 'name' in params")
		}
		tool, ok := s.tools[name]
		if !ok {
			return rpcErr(req.ID, -32601, "Tool not found: "+name)
		}
		arguments := map[string]interface{}{}
		if rawArgs, ok := req.Params["arguments"].(map[string]interface{}); ok {
			arguments = rawArgs
		}
		text, isError, err := tool.Handler(arguments)
		if err != nil {
			text = "工具调用异常: " + err.Error()
			isError = true
		}
		return JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": text},
				},
				"isError": isError,
			},
		}
	default:
		return rpcErr(req.ID, -32601, "Unknown method: "+req.Method)
	}
}

func rpcErr(id interface{}, code int, msg string) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
