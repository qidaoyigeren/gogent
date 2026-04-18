package embedding

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

// siliconFlowBatchSize SiliconFlow API 的批量大小限制
// 超过此数量需要分批发送，避免单次请求过大
const siliconFlowBatchSize = 32

type SiliconFlowClient struct {
	httpClient *http.Client // HTTP 客户端，复用连接池
}

func NewSiliconFlowClient() *SiliconFlowClient {
	return &SiliconFlowClient{
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *SiliconFlowClient) Provider() string { return "siliconflow" }

// sfEmbeddingRequest SiliconFlow API 的请求体结构
// 对应 API 文档：https://docs.siliconflow.cn/api-reference/embeddings/create-embeddings
type sfEmbeddingRequest struct {
	Model string   `json:"model"` // 模型名称，如 "BAAI/bge-large-zh-v1.5"
	Input []string `json:"input"` // 需要转换为向量的文本列表
}

// sfEmbeddingResponse SiliconFlow API 的响应体结构
type sfEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"` // 向量数据
	} `json:"data"`
}

// Embed 实现 EmbeddingClient 接口
// 批量获取文本向量，支持自动分批处理
//
// 工作流程：
// 1. 将 texts 按 siliconFlowBatchSize（32）分批
// 2. 对每批调用 embedBatch 方法
// 3. 合并所有批次的结果
// 4. 返回完整的向量列表
//
// 为什么需要分批？
// - API 限制：某些 provider 对单次请求的文本数量有限制
// - 性能优化：避免单次请求过大导致超时
// - 错误恢复：某批失败不影响其他批次
func (c *SiliconFlowClient) Embed(ctx context.Context, target model.ModelTarget, texts []string) ([][]float32, error) {
	var allEmbeddings [][]float32

	for i := 0; i < len(texts); i += siliconFlowBatchSize {
		end := i + siliconFlowBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]
		embeddings, err := c.embedBatch(ctx, target, batch)
		if err != nil {
			return nil, fmt.Errorf("batch %d-%d failed: %w", i, end, err)
		}
		allEmbeddings = append(allEmbeddings, embeddings...)
	}
	return allEmbeddings, nil
}

// embedBatch 处理单个批次的 embedding 请求
func (c *SiliconFlowClient) embedBatch(ctx context.Context, target model.ModelTarget, texts []string) ([][]float32, error) {
	reqBody := sfEmbeddingRequest{Model: target.Model, Input: texts}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("siliconflow embedding returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp sfEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	result := make([][]float32, len(apiResp.Data))
	for i, d := range apiResp.Data {
		result[i] = d.Embedding
	}
	return result, nil
}
