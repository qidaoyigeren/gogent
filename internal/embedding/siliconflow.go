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

// SiliconFlowClient 实现 EmbeddingClient 接口，用于调用 SiliconFlow 云端 API
// SiliconFlow 是一个提供多种 embedding 模型的云平台
type SiliconFlowClient struct {
	httpClient *http.Client // HTTP 客户端，复用连接池
}

// NewSiliconFlowClient 创建 SiliconFlow 客户端
func NewSiliconFlowClient() *SiliconFlowClient {
	return &SiliconFlowClient{
		// 设置 60 秒超时，防止请求挂起
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// Provider 返回提供商名称
// 用于路由层根据此名称选择对应的客户端
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

	// 分批处理：每批最多 siliconFlowBatchSize 条文本
	for i := 0; i < len(texts); i += siliconFlowBatchSize {
		end := i + siliconFlowBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		// 调用批次处理方法
		embeddings, err := c.embedBatch(ctx, target, batch)
		if err != nil {
			// 批次失败，返回错误（包含批次范围信息）
			return nil, fmt.Errorf("batch %d-%d failed: %w", i, end, err)
		}
		allEmbeddings = append(allEmbeddings, embeddings...)
	}

	return allEmbeddings, nil
}

// embedBatch 处理单个批次的 embedding 请求
// 参数：
//   - ctx: 上下文，支持取消和超时
//   - target: 模型目标（URL、API Key、模型名）
//   - texts: 文本列表（最多 32 条）
//
// 工作流程：
// 1. 构建请求体（JSON）
// 2. 创建 HTTP 请求（POST）
// 3. 设置请求头（Content-Type、Authorization）
// 4. 发送请求
// 5. 检查响应状态码
// 6. 解析响应体（JSON）
// 7. 提取向量数据
func (c *SiliconFlowClient) embedBatch(ctx context.Context, target model.ModelTarget, texts []string) ([][]float32, error) {
	// 1. 构建请求体
	reqBody := sfEmbeddingRequest{Model: target.Model, Input: texts}
	body, _ := json.Marshal(reqBody)

	// 2. 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 3. 设置请求头
	req.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		// Bearer Token 鉴权
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	// 4. 发送请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 5. 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("siliconflow embedding returned %d: %s", resp.StatusCode, string(respBody))
	}

	// 6. 解析响应体
	var apiResp sfEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	// 7. 提取向量数据
	result := make([][]float32, len(apiResp.Data))
	for i, d := range apiResp.Data {
		result[i] = d.Embedding
	}
	return result, nil
}
