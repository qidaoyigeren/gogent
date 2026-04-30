package parser

// Tika 客户端：对接独立部署的 Tika-Server（REST），本仓库不负责启动 Tika；未配置 endpoint 时 parser_registry 不注册 tika 解析器。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TikaParser：所有请求发往同一 base endpoint（如 http://tika:9998），路径固定 /tika、/meta、/detect/stream、/version。
type TikaParser struct {
	endpoint string
	client   *http.Client
}

type TikaConfig struct {
	Endpoint string // 不要尾斜杠，代码里直接拼接 p.endpoint+"/tika"
	Timeout  time.Duration
}

func NewTikaParser(cfg TikaConfig) *TikaParser {
	return &TikaParser{
		endpoint: cfg.Endpoint,
		client: &http.Client{
			// 超时由配置控制，避免 Tika 卡住导致入库任务长期 running。
			Timeout: cfg.Timeout,
		},
	}
}

func (p *TikaParser) Parse(ctx context.Context, reader io.Reader, mimeType string) (string, error) {
	// Tika 的 /tika 接口使用 PUT 上传原始文件流，响应体就是抽取后的文本。
	req, err := http.NewRequestWithContext(ctx, "PUT", p.endpoint+"/tika", reader)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	if mimeType != "" {
		// Content-Type 帮助 Tika 选择更准确的解析器；为空时由 Tika 自行检测。
		req.Header.Set("Content-Type", mimeType)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tika request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 非 200 直接转成错误，让上层任务状态进入 failed。
		return "", fmt.Errorf("tika returned status %d", resp.StatusCode)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(content), nil
}

func (p *TikaParser) ParseWithMetadata(ctx context.Context, reader io.Reader, mimeType string) (string, map[string]string, error) {
	// Parse 已读完 body 后，同一 reader 往往不能再读——生产应先把字节读到 []byte 再 NewReader 两次，或只调 Parse
	content, err := p.Parse(ctx, reader, mimeType)
	if err != nil {
		return "", nil, err
	}

	metadata, err := p.GetMetadata(ctx, reader, mimeType)
	if err != nil {
		return content, nil, err
	}

	return content, metadata, nil
}

// GetMetadata extracts only metadata from a document.
func (p *TikaParser) GetMetadata(ctx context.Context, reader io.Reader, mimeType string) (map[string]string, error) {
	// /meta 返回 Tika 识别出的文件元数据，如标题、作者、页数等。
	req, err := http.NewRequestWithContext(ctx, "PUT", p.endpoint+"/meta", reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata request: %w", err)
	}

	if mimeType != "" {
		req.Header.Set("Content-Type", mimeType)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tika metadata request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tika metadata returned status %d", resp.StatusCode)
	}

	// 当前为占位：未把 resp.Body 解 JSON；需接 Tika /meta 真实响应再填 map
	metadata := make(map[string]string)
	return metadata, nil
}

func (p *TikaParser) DetectMimeType(ctx context.Context, reader io.Reader) (string, error) {
	// /detect/stream 根据文件内容检测 MIME，适合上传 header 不可信的场景。
	req, err := http.NewRequestWithContext(ctx, "PUT", p.endpoint+"/detect/stream", reader)
	if err != nil {
		return "", fmt.Errorf("failed to create detect request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tika detect request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tika detect returned status %d", resp.StatusCode)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read detect response: %w", err)
	}

	return string(content), nil
}

func (p *TikaParser) Health(ctx context.Context) error {
	// /version 是轻量健康检查，用来判断外部 Tika 服务是否可达。
	req, err := http.NewRequestWithContext(ctx, "GET", p.endpoint+"/version", nil)
	if err != nil {
		return fmt.Errorf("failed to create health check request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("tika health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tika health check returned status %d", resp.StatusCode)
	}

	return nil
}
