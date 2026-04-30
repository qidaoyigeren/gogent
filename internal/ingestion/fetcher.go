package ingestion

// Day14 学习注释：fetcher 是流水线的**首个数据面节点**，负责把多源内容收敛为 RawBytes。
//
// 职责边界：只取字节、补全 FileName/MimeType/SourceURL 等元信息，不做 Tika/分块/向量。
//   - file：读本地或 FileStore 已落盘路径（与上传 API + main 中 FileStore 配合）
//   - url：HTTP(S) 拉取，支持 credentials 里以 header: 前缀注入鉴权头
//   - s3：用 AWS SDK 从 bucket+key 拉对象
//   - feishu：tenant_access_token + 导出/下载 API，适合对接飞书文档
// Execute 若发现 ingestCtx 已带 RawBytes（例如 multipart 直传），则跳过外网 IO，只补齐派生字段。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type FetcherNode struct {
	httpClient *http.Client
}

// NewFetcherNode 创建抓取节点，默认 HTTP 超时 60 秒。
// 本节点只负责拿到原始字节，不做文本解析和分块。
func NewFetcherNode() *FetcherNode {
	return &FetcherNode{
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (n *FetcherNode) Name() string { return "fetcher" }

// Execute 根据 SourceType 把文档来源读取成 RawBytes。
// 上传接口可能已经把文件字节放进 ingestCtx.RawBytes，此时直接补齐文件名/MIME；
// 否则按 file/url/s3/feishu 分发到具体抓取方法。
func (n *FetcherNode) Execute(ctx context.Context, ingestCtx *IngestionContext, _ NodeConfig) NodeResult {
	if len(ingestCtx.RawBytes) > 0 {
		// 预加载字节通常来自 multipart 上传或 FileStore，不需要再次访问外部来源。
		n.finalizeFetchedContent(ingestCtx, ingestCtx.RawBytes)
		return NewNodeResultWithOutput("抓取完成", map[string]interface{}{
			"sourceType": string(ingestCtx.Source.Type),
			"size":       len(ingestCtx.RawBytes),
		})
	}

	sourceType, err := ParseSourceType(string(ingestCtx.Source.Type))
	if err != nil {
		return NewNodeResultError(err)
	}

	var data []byte
	switch sourceType {
	case SourceTypeFile:
		// 本地文件路径，常用于开发调试或离线任务。
		data, err = n.fetchFile(ingestCtx.Source.Location)
	case SourceTypeURL:
		// 普通 HTTP/HTTPS 来源，可通过 credentials 注入 header。
		data, err = n.fetchHTTP(ctx, ingestCtx.Source.Location, ingestCtx.Source.Credentials)
	case SourceTypeS3:
		// 对象存储来源，支持 s3://bucket/key 或 credentials 中的 bucket/key。
		data, err = n.fetchS3(ctx, ingestCtx.Source.Location, ingestCtx.Source.Credentials)
	case SourceTypeFeishu:
		// 飞书文档需要先换 tenant_access_token，再调用导出接口。
		data, err = n.fetchFeishu(ctx, ingestCtx.Source.Location, ingestCtx.Source.Credentials)
	default:
		err = fmt.Errorf("unsupported source type: %s", sourceType)
	}
	if err != nil {
		return NewNodeResultError(err)
	}

	n.finalizeFetchedContent(ingestCtx, data)
	return NewNodeResultWithOutput("抓取完成", map[string]interface{}{
		"sourceType":     string(sourceType),
		"sourceLocation": ingestCtx.Source.Location,
		"size":           len(data),
		"fileName":       ingestCtx.FileName,
		"mimeType":       ingestCtx.MimeType,
	})
}

// Process 是旧版 Document pipeline 的兼容入口。
func (n *FetcherNode) Process(ctx context.Context, doc *Document) error {
	ingestCtx := &IngestionContext{
		DocID:     doc.ID,
		FileName:  doc.FileName,
		SourceURL: doc.SourceURL,
		Source: DocumentSource{
			Type:     sourceTypeFromLocation(doc.SourceURL),
			Location: doc.SourceURL,
			FileName: doc.FileName,
		},
	}
	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: string(IngestionNodeTypeFetcher)})
	if !result.Success {
		return result.Error
	}
	doc.Content = string(ingestCtx.RawBytes)
	if doc.FileName == "" {
		doc.FileName = ingestCtx.FileName
	}
	if doc.Metadata == nil {
		doc.Metadata = map[string]string{}
	}
	if ingestCtx.MimeType != "" {
		doc.Metadata["content_type"] = ingestCtx.MimeType
	}
	return nil
}

// finalizeFetchedContent 将抓取结果写回上下文，并补齐后续节点依赖的派生字段。
// RawBytes 给 parser 使用，FileName/MimeType/SourceURL 给日志、metadata 和 handler 使用。
func (n *FetcherNode) finalizeFetchedContent(ingestCtx *IngestionContext, data []byte) {
	ingestCtx.RawBytes = data
	if ingestCtx.FileName == "" {
		if ingestCtx.Source.FileName != "" {
			ingestCtx.FileName = ingestCtx.Source.FileName
		} else if ingestCtx.Source.Location != "" {
			ingestCtx.FileName = path.Base(ingestCtx.Source.Location)
		}
	}
	if ingestCtx.MimeType == "" {
		ingestCtx.MimeType = http.DetectContentType(data)
	}
	if ingestCtx.SourceURL == "" {
		ingestCtx.SourceURL = ingestCtx.Source.Location
	}
}

// fetchFile 从本地路径读取文件内容。
func (n *FetcherNode) fetchFile(location string) ([]byte, error) {
	if strings.TrimSpace(location) == "" {
		return nil, fmt.Errorf("file source location is empty")
	}
	data, err := os.ReadFile(location)
	if err != nil {
		return nil, fmt.Errorf("read local file: %w", err)
	}
	return data, nil
}

// fetchHTTP 从 URL 下载内容，并支持 credentials 中 header:* 形式的自定义请求头。
func (n *FetcherNode) fetchHTTP(ctx context.Context, location string, credentials map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range credentials {
		if strings.HasPrefix(strings.ToLower(key), "header:") {
			req.Header.Set(strings.TrimPrefix(key, "header:"), value)
		}
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch HTTP returned %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// fetchS3 使用 AWS SDK 读取对象存储内容。
// endpoint/usePathStyle 让它也能连接 MinIO 等 S3 兼容服务。
func (n *FetcherNode) fetchS3(ctx context.Context, location string, credentialsMap map[string]string) ([]byte, error) {
	bucket, key, err := parseS3Location(location, credentialsMap)
	if err != nil {
		return nil, err
	}
	region := credentialsMap["region"]
	if region == "" {
		region = "us-east-1"
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			credentialsMap["accessKeyId"],
			credentialsMap["secretAccessKey"],
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load s3 config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint := credentialsMap["endpoint"]; endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = strings.EqualFold(credentialsMap["usePathStyle"], "true")
		}
	})
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("fetch s3 object: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// fetchFeishu 读取飞书文档导出内容。
// location 可以是飞书链接，也可以通过 credentials.docToken 直接指定文档 token。
func (n *FetcherNode) fetchFeishu(ctx context.Context, location string, credentialsMap map[string]string) ([]byte, error) {
	baseURL := credentialsMap["baseURL"]
	if baseURL == "" {
		baseURL = credentialsMap["baseUrl"]
	}
	if baseURL == "" {
		baseURL = "https://open.feishu.cn"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	appID := credentialsMap["appId"]
	appSecret := credentialsMap["appSecret"]
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("feishu fetch requires appId and appSecret")
	}
	docToken := credentialsMap["docToken"]
	if docToken == "" {
		docToken = extractFeishuDocToken(location)
	}
	if docToken == "" {
		return nil, fmt.Errorf("unable to resolve feishu doc token")
	}

	tokenReqBody := strings.NewReader(fmt.Sprintf(`{"app_id":"%s","app_secret":"%s"}`, appID, appSecret))
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/open-apis/auth/v3/tenant_access_token/internal", tokenReqBody)
	if err != nil {
		return nil, err
	}
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenResp, err := n.httpClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("fetch feishu tenant token: %w", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return nil, fmt.Errorf("feishu tenant token returned %d: %s", tokenResp.StatusCode, string(body))
	}

	var tokenPayload struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenPayload); err != nil {
		return nil, fmt.Errorf("decode feishu tenant token response: %w", err)
	}
	if tokenPayload.Code != 0 || tokenPayload.TenantAccessToken == "" {
		return nil, fmt.Errorf("feishu tenant token error: %s", tokenPayload.Msg)
	}

	// 第二步用 tenant token 读取 docx 原始文本内容，返回给 parser 节点继续处理。
	docReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/open-apis/docx/v1/documents/"+docToken+"/raw_content", nil)
	if err != nil {
		return nil, err
	}
	docReq.Header.Set("Authorization", "Bearer "+tokenPayload.TenantAccessToken)
	docResp, err := n.httpClient.Do(docReq)
	if err != nil {
		return nil, fmt.Errorf("fetch feishu raw content: %w", err)
	}
	defer docResp.Body.Close()
	if docResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(docResp.Body)
		return nil, fmt.Errorf("feishu raw content returned %d: %s", docResp.StatusCode, string(body))
	}
	var docPayload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.NewDecoder(docResp.Body).Decode(&docPayload); err != nil {
		return nil, fmt.Errorf("decode feishu raw content response: %w", err)
	}
	if docPayload.Code != 0 {
		return nil, fmt.Errorf("feishu raw content error: %s", docPayload.Msg)
	}
	return []byte(docPayload.Data.Content), nil
}

// sourceTypeFromLocation 根据旧版 Document.SourceURL 推断来源类型。
// 这是 legacy Process 路径的辅助函数，新版链路优先使用 DocumentSource.Type。
func sourceTypeFromLocation(location string) SourceType {
	switch {
	case strings.HasPrefix(location, "http://"), strings.HasPrefix(location, "https://"):
		return SourceTypeURL
	case strings.HasPrefix(location, "s3://"):
		return SourceTypeS3
	default:
		return SourceTypeFile
	}
}

// parseS3Location 同时支持 s3://bucket/key 和 credentials 中 bucket/key 两种写法。
// 前者适合配置化来源，后者适合密钥与对象路径分开管理的场景。
func parseS3Location(location string, credentialsMap map[string]string) (string, string, error) {
	if strings.HasPrefix(location, "s3://") {
		trimmed := strings.TrimPrefix(location, "s3://")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid s3 source location: %s", location)
		}
		return parts[0], parts[1], nil
	}
	bucket := credentialsMap["bucket"]
	key := credentialsMap["key"]
	if bucket == "" || key == "" {
		return "", "", fmt.Errorf("s3 source requires s3://bucket/key or bucket/key credentials")
	}
	return bucket, key, nil
}

// extractFeishuDocToken 从飞书链接中提取文档 token；如果不是链接，则把输入本身
// 当作 token 使用，方便任务配置直接填写 docToken。
func extractFeishuDocToken(location string) string {
	if location == "" {
		return ""
	}
	if strings.Contains(location, "open.feishu.cn") || strings.Contains(location, "feishu.cn") {
		parsed, err := url.Parse(location)
		if err == nil {
			parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
			for i, part := range parts {
				if (part == "d" || part == "docs" || part == "documents") && i+1 < len(parts) {
					return parts[i+1]
				}
			}
		}
	}
	return strings.TrimSpace(location)
}
