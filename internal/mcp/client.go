package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ToolCallResult MCP 工具调用结果
type ToolCallResult struct {
	Text string                 // 文本结果
	Data map[string]interface{} // 结构化数据（JSON 对象）
}

// Client MCP 客户端接口
type Client interface {
	// Initialize 建立与 MCP Server 的连接
	Initialize(ctx context.Context) bool
	// ListTools 获取 MCP Server 上的所有可用工具
	ListTools(ctx context.Context) ([]MCPTool, error)
	// CallTool 执行远程工具调用
	CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (ToolCallResult, error)
}

// HTTPClient 基于 HTTP 传输的 MCP 客户端实现
type HTTPClient struct {
	httpClient *http.Client // HTTP 客户端（带超时配置）
	serverURL  string       // MCP Server URL
	requestID  atomic.Int64 // 自增请求 ID（JSON-RPC 需要）
}

// NewHTTPClient 创建新的 HTTP MCP 客户端
// 参数：
// - serverURL: MCP Server 的 URL 地址
// 默认配置：
// - HTTP 超时：30 秒
func NewHTTPClient(serverURL string) *HTTPClient {
	return &HTTPClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		serverURL: serverURL,
	}
}

// Initialize 建立与 MCP Server 的连接
// 工作流程：
// 1. 发送 initialize 请求（包含协议版本、客户端信息）
// 2. 等待服务器响应
// 3. 发送 notifications/initialized 通知（MCP 协议要求）
func (c *HTTPClient) Initialize(ctx context.Context) bool {
	params := map[string]interface{}{
		"protocolVersion": "2026-02-28", // MCP 协议版本
		"clientInfo": map[string]string{
			"name":    "gogent",
			"version": "1.0.0",
		},
	}

	_, err := c.sendRequest(ctx, "initialize", params)
	if err != nil {
		slog.Error("MCP initialization failed, skipping initialized notification", "err", err)
		return false
	}

	// MCP 协议要求：收到 initialize 响应后，必须发送 notifications/initialized
	c.sendInitializedNotification(ctx)
	slog.Info("MCP client initialized successfully", "server", c.serverURL)
	return true
}

// ListTools 获取 MCP Server 上的所有可用工具
// 工作流程：
// 1. 发送 tools/list 请求
// 2. 解析响应中的 tools 数组
// 3. 转换为 MCPTool 结构体列表
func (c *HTTPClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	result, err := c.sendRequest(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	if result == nil {
		return []MCPTool{}, nil
	}
	toolsRaw, ok := result["tools"].([]interface{})
	if !ok {
		return []MCPTool{}, nil
	}

	var tools []MCPTool
	for _, toolRaw := range toolsRaw {
		toolObj, ok := toolRaw.(map[string]interface{})
		if !ok {
			continue
		}
		tool := c.convertToMCPTool(toolObj)
		tools = append(tools, tool)
	}

	slog.Info("MCP tools retrieved", "count", len(tools), "server", c.serverURL)
	return tools, nil
}

// CallTool 执行远程工具调用
// 工作流程：
// 1. 验证参数（toolName 非空）
// 2. 发送 tools/call 请求
// 3. 解析响应（提取文本内容）
// 4. 检查 isError 标志
// 5. 尝试解析 JSON 对象（用于 MCPResponse.Data）
func (c *HTTPClient) CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (ToolCallResult, error) {
	if toolName == "" {
		slog.Warn("MCP tool call failed, toolName is empty")
		return ToolCallResult{}, fmt.Errorf("toolName is empty")
	}

	if arguments == nil {
		arguments = make(map[string]interface{})
	}

	params := map[string]interface{}{
		"name":      toolName,
		"arguments": arguments,
	}
	result, err := c.sendRequest(ctx, "tools/call", params)
	if err != nil {
		return ToolCallResult{}, fmt.Errorf("failed to call tool %s: %w", toolName, err)
	}
	if result == nil {
		return ToolCallResult{}, fmt.Errorf("tool call returned nil result")
	}

	textResult := c.extractTextContent(result)

	isError, _ := result["isError"].(bool)
	if isError {
		slog.Warn("MCP tool call returned error", "tool", toolName, "error", textResult)
		return ToolCallResult{}, fmt.Errorf("tool call error: %s", textResult)
	}
	out := ToolCallResult{Text: textResult}
	if data := parseJSONObjectFromText(textResult); len(data) > 0 {
		out.Data = data
	}
	return out, nil
}

// sendRequest 发送 JSON-RPC 2.0 请求
// 工作流程：
// 1. 构建 JSON-RPC 请求（jsonrpc/id/method/params）
// 2. 解析 MCP endpoint URL（自动添加 /mcp 后缀）
// 3. 发送 HTTP POST 请求
// 4. 检查 HTTP 状态码
// 5. 解析 JSON-RPC 响应
// 6. 检查 JSON-RPC 错误
// 7. 返回 result 字段
//
// 错误处理：
// - HTTP 请求失败：返回错误
// - HTTP 状态码非 2xx：返回错误
// - JSON 解析失败：返回错误
// - JSON-RPC 错误：返回错误
func (c *HTTPClient) sendRequest(ctx context.Context, method string, params map[string]interface{}) (map[string]interface{}, error) {
	rpcRequest := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      c.requestID.Add(1), // 自增 ID
		"method":  method,
		"params":  params,
	}

	url := c.resolveMCPEndpointURL()
	requestBody, err := json.Marshal(rpcRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Error("MCP request failed", "method", method, "url", url, "err", err)
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if !isHTTPSuccess(resp.StatusCode) {
		slog.Warn("MCP request failed", "method", method, "url", url, "status", resp.StatusCode)
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("MCP request failed", "method", method, "url", url, "err", "response body is empty")
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var rpcResponse map[string]interface{}
	if err := json.Unmarshal(body, &rpcResponse); err != nil {
		slog.Error("MCP JSON parse error", "method", method, "url", url, "err", err)
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// 检查 JSON-RPC 错误
	if rpcError, ok := rpcResponse["error"]; ok && rpcError != nil {
		errorObj, _ := rpcError.(map[string]interface{})
		code := errorObj["code"]
		message := errorObj["message"]
		slog.Error("MCP JSON-RPC error", "method", method, "code", code, "message", message)
		return nil, fmt.Errorf("JSON-RPC error %v: %v", code, message)
	}

	// 返回 result 字段
	if result, ok := rpcResponse["result"]; ok {
		resultMap, _ := result.(map[string]interface{})
		return resultMap, nil
	}

	return nil, nil
}

// sendInitializedNotification 发送 JSON-RPC 2.0 通知（无 id，不需要响应）
// MCP 协议要求：initialize 之后必须发送 notifications/initialized
func (c *HTTPClient) sendInitializedNotification(ctx context.Context) {
	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}

	url := c.resolveMCPEndpointURL()
	body, err := json.Marshal(notification)
	if err != nil {
		slog.Warn("Failed to marshal initialized notification", "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("Failed to create initialized notification request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("Failed to send initialized notification", "err", err)
		return
	}
	defer resp.Body.Close()

	if !isHTTPSuccess(resp.StatusCode) {
		slog.Warn("Initialized notification failed", "status", resp.StatusCode)
	}
}

// convertToMCPTool 将 MCP 标准 Tool Schema 转换为 MCPTool
// 转换规则：
// 1. name → ToolID + Name
// 2. description → Description
// 3. inputSchema → Parameters（JSON 格式）
func (c *HTTPClient) convertToMCPTool(toolObj map[string]interface{}) MCPTool {
	name, _ := toolObj["name"].(string)
	description, _ := toolObj["description"].(string)

	// 解析 inputSchema
	var parameters json.RawMessage
	if inputSchemaRaw, ok := toolObj["inputSchema"]; ok {
		if inputSchema, ok := inputSchemaRaw.(map[string]interface{}); ok {
			// 转换为 JSON
			paramsJSON, _ := json.Marshal(inputSchema)
			parameters = paramsJSON
		}
	}

	return MCPTool{
		ToolID:      name,
		Name:        name,
		Description: description,
		Parameters:  parameters,
	}
}

// extractTextContent 从工具调用结果中提取文本内容
// 工作流程：
// 1. 解析 content 字段（数组）
// 2. 遍历数组，提取 type="text" 的 text 字段
// 3. 拼接所有文本段（用换行符分隔）
func (c *HTTPClient) extractTextContent(result map[string]interface{}) string {
	if result == nil {
		return ""
	}

	contentRaw, ok := result["content"]
	if !ok {
		return ""
	}

	contentArray, ok := contentRaw.([]interface{})
	if !ok {
		return ""
	}

	textSegments := make([]string, 0)
	for _, item := range contentArray {
		contentObj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if text, ok := contentObj["text"].(string); ok && text != "" {
			textSegments = append(textSegments, text)
		}
	}

	if len(textSegments) == 0 {
		return ""
	}

	return strings.Join(textSegments, "\n")
}

// parseJSONObjectFromText 尝试将整个文本解析为 JSON 对象
func parseJSONObjectFromText(text string) map[string]interface{} {
	s := strings.TrimSpace(text)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// resolveMCPEndpointURL resolves the MCP endpoint URL.
func (c *HTTPClient) resolveMCPEndpointURL() string {
	if strings.HasSuffix(c.serverURL, "/mcp") {
		return c.serverURL
	}
	return c.serverURL + "/mcp"
}

func isHTTPSuccess(statusCode int) bool {
	return statusCode >= 200 && statusCode < 300
}
