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

type BaiLianRerankClient struct {
	httpClient *http.Client // HTTP 客户端，复用连接池
}

func NewBaiLianRerankClient() *BaiLianRerankClient {
	return &BaiLianRerankClient{
		// 设置 30 秒超时（Rerank 通常较快）
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *BaiLianRerankClient) Provider() string { return "bailian" }

type bailianRerankRequest struct {
	Model      string             `json:"model"`                // 模型名称，如 "gte-rerank"
	Input      bailianRerankInput `json:"input"`                // 输入数据
	Parameters map[string]int     `json:"parameters,omitempty"` // 可选参数
}

type bailianRerankInput struct {
	Query     string   `json:"query"`     // 查询文本
	Documents []string `json:"documents"` // 待排序的文档列表
}

type bailianRerankResponse struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`           // 文档在输入中的索引
			RelevanceScore float64 `json:"relevance_score"` // 相关性分数
		} `json:"results"`
	} `json:"output"`
}

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
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	// 3. 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 4. 设置请求头
	req.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
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
			text = documents[r.Index]
		}
		results[i] = RerankResult{
			Text:  text,
			Score: r.RelevanceScore,
			Index: r.Index,
		}
	}
	return results, nil
}
