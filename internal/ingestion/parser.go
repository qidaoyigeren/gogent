package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	internalparser "gogent/internal/parser"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ParserSettings 节点级 JSON：Type 非空时**强制**用该名字解析器；Rules 为 MIME 白名单。
type ParserSettings struct {
	Type  string       `json:"type"`
	Rules []ParserRule `json:"rules"`
}

// ParserRule 仅用到 MimeType；Options 预留给各 parser 扩展，当前未统一消费。
type ParserRule struct {
	MimeType string                 `json:"mimeType"`
	Options  map[string]interface{} `json:"options"`
}

// ParserNode 内挂一组 DocumentParser（markdown/html/text/tika 等），由 selectParser 择一执行。
type ParserNode struct {
	parsers []DocumentParser
}

// NewParserNode 创建解析节点并注册默认 parser。
// Tika 可为空；为空时只使用内置 text/html parser，适合测试或纯文本场景。
func NewParserNode(tika *internalparser.TikaParser) *ParserNode {
	return &ParserNode{parsers: newDefaultDocumentParsers(tika)}
}

func (n *ParserNode) Name() string { return "parser" }

// Execute 将 fetcher 放入 RawBytes 的原始内容解析成 RawText。
// 它负责 MIME 检测、按配置校验允许的类型、选择具体 parser、清洗文本，并合并
// parser 产出的 metadata。后续 chunker 只依赖这里写入的 RawText/Document。
func (n *ParserNode) Execute(ctx context.Context, ingestCtx *IngestionContext, config NodeConfig) NodeResult {
	if len(ingestCtx.RawBytes) == 0 && ingestCtx.RawText != "" {
		// 管理端/测试可能直接塞 RawText，跳过反复解析，避免 Tika 再跑一遍
		return NewNodeResultWithOutput("解析完成", map[string]interface{}{
			"mimeType":   ingestCtx.MimeType,
			"parsedSize": len(ingestCtx.RawText),
			"parserType": "preloaded",
		})
	}
	if len(ingestCtx.RawBytes) == 0 {
		return NewNodeResultError(fmt.Errorf("解析器缺少原始字节"))
	}
	if ingestCtx.MimeType == "" {
		// fetcher 未识别类型时，用 Go 标准库基于文件头做兜底检测。
		ingestCtx.MimeType = httpDetectMime(ingestCtx.RawBytes)
	}

	settings := parseParserSettings(config.Settings)
	if !matchesParserRules(ingestCtx.MimeType, settings.Rules) {
		// rules 是 pipeline 的安全阀：不允许的 MIME 直接失败，避免误解析未知文件。
		return NewNodeResultError(fmt.Errorf("mime type %s is not allowed by parser settings", ingestCtx.MimeType))
	}

	parser, err := n.selectParser(settings, ingestCtx.MimeType, ingestCtx.RawBytes)
	if err != nil {
		return NewNodeResultError(err)
	}
	parsed, err := parser.Parse(ctx, ingestCtx.RawBytes, ingestCtx.MimeType)
	if err != nil {
		return NewNodeResultError(err)
	}
	// 清洗后的 RawText 是后续 chunker、enhancer 的标准输入。
	ingestCtx.RawText = TextCleanup(parsed.Text)
	if ingestCtx.Document == nil {
		ingestCtx.Document = &StructuredDocument{}
	}
	ingestCtx.Document.Content = ingestCtx.RawText
	merged := map[string]interface{}{}
	for k, v := range ingestCtx.Metadata {
		merged[k] = v
	}
	for k, v := range parsed.Metadata {
		// 覆盖同名键：解析器（尤其 Tika）对 title、author 等更可信
		merged[k] = v
	}
	ingestCtx.Document.Metadata = merged
	ingestCtx.Metadata = merged
	return NewNodeResultWithOutput("解析完成", map[string]interface{}{
		"mimeType":   ingestCtx.MimeType,
		"parsedSize": len(ingestCtx.RawText),
		"parserType": parsed.Type,
	})
}

// Process 适配旧版 Document pipeline，把旧结构包装成 IngestionContext 后复用 Execute。
func (n *ParserNode) Process(ctx context.Context, doc *Document) error {
	metadata := map[string]interface{}{}
	if doc.Metadata != nil {
		for k, v := range doc.Metadata {
			metadata[k] = v
		}
	}
	ingestCtx := &IngestionContext{
		DocID:    doc.ID,
		FileName: doc.FileName,
		MimeType: doc.Metadata["content_type"],
		RawBytes: []byte(doc.Content),
		Metadata: metadata,
	}
	result := n.Execute(ctx, ingestCtx, NodeConfig{NodeType: string(IngestionNodeTypeParser)})
	if !result.Success {
		return result.Error
	}
	doc.Parsed = ingestCtx.RawText
	return nil
}

// selectParser 根据 settings.Type 或 MIME/内容探测选择具体 parser。
// 显式指定 parser 时必须存在；未指定时按注册顺序选择第一个 Supports 返回 true 的实现。
func (n *ParserNode) selectParser(settings ParserSettings, mimeType string, raw []byte) (DocumentParser, error) {
	preferred := strings.ToLower(strings.TrimSpace(settings.Type))
	if preferred != "" {
		// 显式 type：按 Name() 精确匹配，未注册则**硬错**，避免默默落到错误实现
		for _, parser := range n.parsers {
			if strings.EqualFold(parser.Name(), preferred) {
				return parser, nil
			}
		}
		return nil, fmt.Errorf("parser type %s is not registered", settings.Type)
	}
	// 自动：按**注册顺序**取第一个 Supports；故 newDefaultDocumentParsers 中顺序决定优先级
	for _, parser := range n.parsers {
		if parser.Supports(mimeType, raw) {
			return parser, nil
		}
	}
	return nil, fmt.Errorf("no parser available for mime type %s", mimeType)
}

// parseParserSettings 读取 parser 节点配置；配置为空时表示自动选择 parser。
func parseParserSettings(raw json.RawMessage) ParserSettings {
	var settings ParserSettings
	if len(raw) == 0 || string(raw) == "null" {
		return settings
	}
	_ = json.Unmarshal(raw, &settings)
	return settings
}

// matchesParserRules 判断当前 MIME 是否被节点配置允许。
// 支持精确 MIME、通配符 image/* 以及 all/default 这类宽松配置。
func matchesParserRules(mimeType string, rules []ParserRule) bool {
	if len(rules) == 0 {
		return true
	}
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	for _, rule := range rules {
		candidate := strings.ToLower(strings.TrimSpace(rule.MimeType))
		switch candidate {
		case "", "*", "all", "default":
			return true
		case mimeType:
			return true
		}
		// 例如 image/* 匹配 image/png（TrimSuffix 去掉 * 后前缀匹配）
		if strings.HasSuffix(candidate, "/*") && strings.HasPrefix(mimeType, strings.TrimSuffix(candidate, "*")) {
			return true
		}
	}
	return false
}

// httpDetectMime 用文件头推断 MIME，是 fetcher 未填 MimeType 时的兜底方案。
func httpDetectMime(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return http.DetectContentType(raw)
}

// TextCleanup：去 UTF-8 BOM、统一换行、行尾去空白、把 3+ 空行压成 2 行、整体 Trim。
func TextCleanup(text string) string {
	if utf8.ValidString(text) && strings.HasPrefix(text, "\xEF\xBB\xBF") {
		text = strings.TrimPrefix(text, "\xEF\xBB\xBF")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// 只去掉行尾空白，尽量保留行首缩进和列表结构。
		lines[i] = strings.TrimRight(line, " \t")
	}
	text = strings.Join(lines, "\n")
	re := regexp.MustCompile(`\n{3,}`)
	text = re.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// extractTextFromHTML：去脚本样式 → 块级结束标签/ br 变换行 → 剥光标签 → 解常见实体。
func extractTextFromHTML(html string) string {
	reScript := regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</\1>`)
	html = reScript.ReplaceAllString(html, "")
	reBlock := regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|br|hr)[^>]*>`)
	html = reBlock.ReplaceAllString(html, "\n")
	reBlockOpen := regexp.MustCompile(`(?i)<br\s*/?>`)
	html = reBlockOpen.ReplaceAllString(html, "\n")
	reTag := regexp.MustCompile(`<[^>]+>`)
	html = reTag.ReplaceAllString(html, "")
	html = strings.ReplaceAll(html, "&amp;", "&")
	html = strings.ReplaceAll(html, "&lt;", "<")
	html = strings.ReplaceAll(html, "&gt;", ">")
	html = strings.ReplaceAll(html, "&quot;", `"`)
	html = strings.ReplaceAll(html, "&#39;", "'")
	html = strings.ReplaceAll(html, "&nbsp;", " ")
	return html
}
