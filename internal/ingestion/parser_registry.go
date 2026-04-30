package ingestion

import (
	"bytes"
	"context"
	"fmt"
	internalparser "gogent/internal/parser"
	"strings"
	"unicode/utf8"
)

//parser_registry.go 定义了 DocumentParser 接口和内置 parser 的注册机制。
// 优先级设计（关键）：
//   markdown → html → text → tika
//   1. markdown/text/html 是“快速路径”，内置实现无外部依赖，响应快、成本低
//   2. 常见文本类格式不会绕过快速路径直接转发给 Tika（Tika 启动和调用成本高）
//   3. Tika 放最后作为“重武器兜底”，专门处理 PDF、Office 等二进制文档，内置 parser 无法识别时才调用

type ParserOutput struct {
	Text     string
	Metadata map[string]interface{}
	Type     string
}

type DocumentParser interface {
	Name() string
	Supports(mimeType string, raw []byte) bool
	Parse(ctx context.Context, raw []byte, mimeType string) (*ParserOutput, error)
}

// newDefaultDocumentParsers 按优先级注册内置 parser。
//
// 注册顺序决定匹配优先级：
//  1. markdown：匹配 MIME 含 "markdown" 的文档，直接转字符串（轻量）
//  2. html：匹配 MIME 含 "html" 的文档，清理标签并保留块级换行（轻量）
//  3. text：匹配 text/plain、其他 text/*、或 UTF-8 有效字节（轻量兜底）
//  4. tika：仅当 tika != nil 时注册，支持所有非空二进制内容（重量级兜底）
//
// markdown/text/html 能快速处理常见文本类内容；Tika 放在最后作为二进制文档兜底。
func newDefaultDocumentParsers(tika *internalparser.TikaParser) []DocumentParser {
	parsers := []DocumentParser{
		textDocumentParser{name: "markdown", match: func(mimeType string, _ []byte) bool {
			return strings.Contains(mimeType, "markdown")
		}},
		htmlDocumentParser{},
		textDocumentParser{name: "text", match: func(mimeType string, raw []byte) bool {
			return strings.Contains(mimeType, "text/plain") || strings.HasPrefix(mimeType, "text/") || utf8.Valid(raw)
		}},
	}
	if tika != nil {
		parsers = append(parsers, tikaDocumentParser{tika: tika})
	}
	return parsers
}

// textDocumentParser 是通用文本解析器，支持 markdown 和 plain text。
// 它不包含外部调用，直接把原始字节转为字符串，因此速度极快。

type textDocumentParser struct {
	name  string
	match func(mimeType string, raw []byte) bool
}

func (p textDocumentParser) Name() string { return p.name }

// Supports 由构造时注入的 match 函数判断是否支持当前 MIME/内容。
// 对于 markdown：匹配 MIME 含 "markdown"
// 对于 text：匹配 text/plain、其他 text/*、或 UTF-8 有效字节
func (p textDocumentParser) Supports(mimeType string, raw []byte) bool {
	if p.match == nil {
		return false
	}
	return p.match(strings.ToLower(strings.TrimSpace(mimeType)), raw)
}

// Parse 对文本类内容不做外部调用，直接把字节转成字符串。
// 这是最轻量级的解析方式，适合 markdown、txt 等纯文本格式。
func (p textDocumentParser) Parse(_ context.Context, raw []byte, _ string) (*ParserOutput, error) {
	return &ParserOutput{Text: string(raw), Type: p.name}, nil
}

// htmlDocumentParser 是 HTML 文档解析器，负责清理标签并保留结构换行。

type htmlDocumentParser struct{}

func (htmlDocumentParser) Name() string { return "html" }

// Supports 通过 MIME 判断 HTML，避免把普通文本误走标签清理逻辑。
// 匹配条件：MIME 类型中包含 "html" 字符串（如 text/html, application/xhtml+xml）
func (htmlDocumentParser) Supports(mimeType string, _ []byte) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(mimeType)), "html")
}

// Parse 使用 extractTextFromHTML 删除标签并保留块级换行。
// 转换规则：
//   - <br>, <p>, </div> 等块级标签转为换行符，保持段落结构
//   - 其他 HTML 标签直接移除，只保留纯文本内容
func (htmlDocumentParser) Parse(_ context.Context, raw []byte, _ string) (*ParserOutput, error) {
	return &ParserOutput{Text: extractTextFromHTML(string(raw)), Type: "html"}, nil
}

// tikaDocumentParser 是 Tika 外部解析器的适配器，用于处理 PDF、Office 等二进制文档。
// 它是重量级解析器，依赖 Apache Tika HTTP 服务，响应慢但支持格式广。

type tikaDocumentParser struct {
	tika *internalparser.TikaParser
}

func (t tikaDocumentParser) Name() string { return "tika" }

// Supports 只要求 Tika 客户端存在且内容非空，因此它是复杂文件的兜底 parser。
// 匹配条件：tika != nil && len(raw) > 0
// 这意味着只要 Tika 服务可用，任何非空二进制内容都会交给它处理。
func (t tikaDocumentParser) Supports(_ string, raw []byte) bool {
	return t.tika != nil && len(raw) > 0
}

// Parse 调用 Apache Tika HTTP 服务，把 PDF/Office 等二进制内容转换成文本。
//
// 失败处理：
//   - Tika 超时、网络错误、解析失败等都会返回 error
//   - 调用方（parser.go）会捕获错误并决定降级策略
func (t tikaDocumentParser) Parse(ctx context.Context, raw []byte, mimeType string) (*ParserOutput, error) {
	text, err := t.tika.Parse(ctx, bytes.NewReader(raw), mimeType)
	if err != nil {
		return nil, fmt.Errorf("tika parse failed: %w", err)
	}
	return &ParserOutput{Text: text, Type: "tika"}, nil
}
