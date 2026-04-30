package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/model"
	"io"
	"net/http"
	"time"
)

// BaiLianRerankClient 实现 RerankClient 接口，用于调用阿里云百炼 Rerank API
// 百炼是阿里云提供的 AI 服务平台，支持多种 Rerank 模型
type BaiLianRerankClient struct {
	httpClient *http.Client // HTTP 客户端，复用连接池
}

// NewBaiLianRerankClient 创建百炼 Rerank 客户端
func NewBaiLianRerankClient() *BaiLianRerankClient {
	return &BaiLianRerankClient{
		// 设置 30 秒超时（Rerank 通常较快）
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Provider 返回提供商名称
func (c *BaiLianRerankClient) Provider() string { return "bailian" }

// bailianRerankRequest 百炼 Rerank API 的请求体结构
type bailianRerankRequest struct {
	Model      string             `json:"model"`                // 模型名称，如 "gte-rerank"
	Input      bailianRerankInput `json:"input"`                // 输入数据
	Parameters map[string]int     `json:"parameters,omitempty"` // 可选参数
}

// bailianRerankInput 百炼 Rerank 的输入数据
type bailianRerankInput struct {
	Query     string   `json:"query"`     // 查询文本
	Documents []string `json:"documents"` // 待排序的文档列表
}

// bailianRerankResponse 百炼 Rerank API 的响应体结构
type bailianRerankResponse struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`           // 文档在输入中的索引
			RelevanceScore float64 `json:"relevance_score"` // 相关性分数
		} `json:"results"`
	} `json:"output"`
}

// Rerank 实现 RerankClient 接口
// 调用百炼 Rerank API 对文档进行重排序
//
// 工作流程：
// 1. 构建请求体（model、query、documents）
// 2. 创建 HTTP 请求（POST）
// 3. 设置请求头（Content-Type、Authorization）
// 4. 发送请求
// 5. 检查响应状态码
// 6. 解析响应体（JSON）
// 7. 提取结果，匹配原文本
// 8. 返回排序后的结果
func (c *BaiLianRerankClient) Rerank(ctx context.Context, target model.ModelTarget, query string, documents []string, topN int) ([]RerankResult, error) {
	// 1. 构建请求体
	reqBody := bailianRerankRequest{
		Model: target.Model,
		Input: bailianRerankInput{
			Query:     query,
			Documents: documents,
		},
	}
	// 设置 topN 参数（可选）
	if topN > 0 {
		reqBody.Parameters = map[string]int{"top_n": topN}
	}

	// 2. 序列化 JSON
	body, _ := json.Marshal(reqBody)

	// 3. 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 4. 设置请求头
	req.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		// Bearer Token 鉴权
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	// 5. 发送请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 6. 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bailian rerank returned %d: %s", resp.StatusCode, string(respBody))
	}

	// 7. 解析响应体
	var apiResp bailianRerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	// 8. 提取结果，匹配原文本
	results := make([]RerankResult, len(apiResp.Output.Results))
	for i, r := range apiResp.Output.Results {
		text := ""
		if r.Index < len(documents) {
			text = documents[r.Index] // 根据索引获取原文本
		}
		results[i] = RerankResult{
			Index: r.Index,
			Score: r.RelevanceScore,
			Text:  text,
		}
	}
	return results, nil
}
