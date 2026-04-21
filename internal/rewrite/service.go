package rewrite

import (
	"context"
	"gogent/internal/chat"
)

// RewriteResult 查询改写结果
// 核心职责：
// 1. Rewritten: 改写后的查询（更适合检索）
// 2. SubQuestions: 拆分后的子问题列表（复杂问题）
type RewriteResult struct {
	Rewritten    string   `json:"rewritten"`
	SubQuestions []string `json:"subQuestions,omitempty"`
}

// QueryRewriteService 查询改写服务接口
// 核心职责：
// 1. 将用户自然语言问题改写为更适合向量检索的查询
// 2. 支持单查询改写和多问题拆分
// 3. 补全指代词、删除无关指令、保留关键限制
type QueryRewriteService interface {
	// Rewrite 改写单个查询（不返回子问题）
	Rewrite(ctx context.Context, query string, history []chat.Message) (*RewriteResult, error)

	// RewriteMulti 改写查询并拆分子问题
	RewriteMulti(ctx context.Context, query string, history []chat.Message) (*RewriteResult, error)
}
