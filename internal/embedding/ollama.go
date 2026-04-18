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

// OllamaEmbeddingClient 实现 EmbeddingClient 接口，用于调用 Ollama 本地模型
type OllamaEmbeddingClient struct {
	httpClient *http.Client // HTTP 客户端，复用连接池
}

func NewOllamaEmbeddingClient() *OllamaEmbeddingClient {
	return &OllamaEmbeddingClient{
		// 设置 60 秒超时
		// 本地模型可能较慢，需要较长的超时时间
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *OllamaEmbeddingClient) Provider() string { return "ollama" }

type ollamaEmbedRequest struct {
	Model string   `json:"model"` // 模型名称，如 "nomic-embed-text"
	Input []string `json:"input"` // 需要转换为向量的文本列表
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"` // 向量数据（二维数组）
}

func (c *OllamaEmbeddingClient) Embed(ctx context.Context, target model.ModelTarget, texts []string) ([][]float32, error) {
	// 1. 构建请求体
	reqBody := ollamaEmbedRequest{Model: target.Model, Input: texts}
	body, _ := json.Marshal(reqBody)

	// 2. 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 3. 设置请求头
	req.Header.Set("Content-Type", "application/json")
	// 注意：Ollama 本地服务无需 API Key，不设置 Authorization

	// 4. 发送请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 5. 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed returned %d: %s", resp.StatusCode, string(respBody))
	}

	// 6. 解析响应体
	var apiResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	// 7. 直接返回 embeddings（无需额外提取）
	return apiResp.Embeddings, nil
}
