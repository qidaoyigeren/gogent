package rewrite

import (
	"context"
	"gogent/internal/chat"
	"gogent/internal/config"
	"gogent/pkg/llmutil"
	"log/slog"
	"strings"
)

// MultiQuestionRewriteService 基于 LLM + 规则的查询改写服务
// 核心职责：
// 1. 调用 LLM 改写查询（补全指代、删除无关指令）
// 2. 拆分复杂问题为多个子问题
// 3. 降级策略：LLM 失败时使用术语映射 + 规则拆分
type MultiQuestionRewriteService struct {
	llm        chat.LLMService           // LLM 服务（调用改写）
	termMapper *TermMapper               // 术语映射器（标准化术语）
	cfg        config.QueryRewriteConfig // 配置（是否启用 LLM）
}

func NewMultiQuestionRewriteService(llm chat.LLMService, termMapper *TermMapper, cfg config.QueryRewriteConfig) *MultiQuestionRewriteService {
	return &MultiQuestionRewriteService{
		llm:        llm,
		termMapper: termMapper,
		cfg:        cfg,
	}
}

const systemPrompt = `# 角色
你是查询改写助手，用于 RAG 检索阶段。

# 任务
1. 将用户问题改写成适合检索的自然语言查询
2. 判断是否需要拆分成多个子问题

# 输出格式
严格返回 JSON，不要额外文字：
{
  "rewrite": "改写后的查询",
  "sub_questions": ["子问题1", "子问题2"]
}

# 改写规则
- 保留专有名词（系统名、产品名、模块名等），原样保留
- 保留关键限制：时间范围、环境、终端类型、角色身份
- 删除礼貌用语："请帮我"、"麻烦"、"谢谢"
- 删除无关回答指令："详细说明"、"分点回答"
- 补全指代词（"它"、"这个"）为具体实体（结合历史）
- 不得添加原文没有的条件

# 拆分规则
- 只在多个问号、显式列举、分号/换行分隔时才拆分
- 若不拆分：sub_questions 只包含 1 条，与 rewrite 完全一致
- 问候/身份类问题保持原样`

// Rewrite 改写单个查询（不返回子问题）
func (s *MultiQuestionRewriteService) Rewrite(ctx context.Context, query string, history []chat.Message) (*RewriteResult, error) {
	result, err := s.rewriteAndSplit(ctx, query, history)
	if err != nil {
		return nil, err
	}
	return &RewriteResult{Rewritten: result.Rewritten}, nil
}

// RewriteMulti 改写查询并拆分子问题（单次 LLM 调用）
func (s *MultiQuestionRewriteService) RewriteMulti(ctx context.Context, query string, history []chat.Message) (*RewriteResult, error) {
	return s.rewriteAndSplit(ctx, query, history)
}

// 工作流程：
// 1. 术语映射（始终执行，免费）
// 2. 检查配置是否启用 LLM
// 3. 构建消息（系统提示 + 最近 4 轮历史 + 当前查询）
// 4. 调用 LLM（temperature=0.1, top_p=0.3）
// 5. 解析 JSON 结果
// 6. 降级处理（LLM 失败或解析失败）
func (s *MultiQuestionRewriteService) rewriteAndSplit(ctx context.Context, query string, history []chat.Message) (*RewriteResult, error) {
	// 始终先应用术语映射（基于规则，无成本）
	normalized := s.termMapper.Normalize(query)
	// 当查询改写被禁用时，跳过 LLM
	// 直接返回术语映射结果 + 基于规则的拆分
	if !s.cfg.Enabled {
		return &RewriteResult{
			Rewritten:    normalized,
			SubQuestions: ruleBasedSplit(normalized),
		}, nil
	}
	// 构建消息：系统提示 + 最近 4 轮历史 + 当前查询
	// 历史裁剪策略：保留最近 4 轮（8 条消息），避免上下文过长
	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}
	if len(history) > 0 {
		start := len(history) - 4
		if start < 0 {
			start = 0
		}
		for _, h := range history[start:] {
			if h.Role == "user" || h.Role == "assistant" {
				messages = append(messages, h)
			}
		}
	}
	messages = append(messages, chat.Message{Role: "user", Content: normalized})
	// 调用 LLM（低温度保证输出稳定）
	resp, err := s.llm.Chat(ctx, messages,
		chat.WithTemperature(0.1),
		chat.WithTopP(0.3),
	)
	if err != nil {
		slog.Warn("rewrite LLM call failed, using normalised query", "err", err)
		return &RewriteResult{
			Rewritten:    normalized,
			SubQuestions: []string{normalized},
		}, nil
	}
	// 提取思考内容（支持 DeepThinking 模型）
	content := llmutil.ExtractThinkContent(resp.Content)
	type llmResult struct {
		Rewrite      string   `json:"rewrite"`       // 改写后的查询
		SubQuestions []string `json:"sub_questions"` // 子问题列表
	}
	parsed, parseErr := llmutil.ParseJSON[llmResult](content)
	if parseErr != nil || strings.TrimSpace(parsed.Rewrite) == "" {
		// JSON 解析失败或改写结果为空 → 降级为术语映射结果
		slog.Warn("rewrite parse failed, falling back", "err", parseErr, "raw", content)
		return &RewriteResult{
			Rewritten:    normalized,
			SubQuestions: []string{normalized},
		}, nil
	}
	// 处理子问题：如果 LLM 未返回子问题，使用改写结果作为唯一子问题
	subs := parsed.SubQuestions
	if len(subs) == 0 {
		subs = []string{parsed.Rewrite}
	}
	slog.Debug("query rewrite", "original", query, "normalised", normalized,
		"rewritten", parsed.Rewrite, "subQuestions", subs)
	return &RewriteResult{
		Rewritten:    parsed.Rewrite, // LLM 改写结果
		SubQuestions: subs,           // 子问题列表
	}, nil
}

// ruleBasedSplit 基于规则的查询拆分（LLM 失败时的降级方案）
// 拆分规则：
// 1. 按中英文分隔符拆分：? ？ 。 ； ; \n
// 2. 每个部分如果不是问号结尾，自动添加问号
// 3. 如果拆分结果为空，返回原始查询
func ruleBasedSplit(question string) []string {
	parts := strings.FieldsFunc(question, func(r rune) bool {
		return r == '?' || r == '？' || r == '。' || r == '；' || r == ';' || r == '\n'
	})
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "?") && !strings.HasSuffix(p, "？") {
			p += "?"
		}
		result = append(result, p)
	}
	if len(result) == 0 {
		return []string{question}
	}
	return result
}
