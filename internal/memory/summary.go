package memory

import (
	"context"
	"fmt"
	"gogent/internal/chat"
	"gogent/internal/config"
	"gogent/pkg/llmutil"
	"log/slog"
)

// SummaryService 对话摘要压缩服务
type SummaryService struct {
	llm   chat.LLMService     // LLM 服务（生成摘要）
	store *Store              // 存储层（读写摘要）
	cfg   config.MemoryConfig // 配置（阈值、开关等）
}

func NewSummaryService(llm chat.LLMService, store *Store, cfg config.MemoryConfig) *SummaryService {
	return &SummaryService{llm: llm, store: store, cfg: cfg}
}

const summaryPrompt = `请将以下对话历史压缩为一段简洁的摘要，保留关键信息和上下文：

对话历史：
%s

要求：
1. 摘要不超过%d个字符
2. 保留关键实体、数字和结论
3. 使用第三人称
4. 只返回摘要文本`

// CompressIfNeeded 检查是否需要压缩并执行摘要生成
//
// 触发时机：Append 方法在追加 ASSISTANT 消息后异步调用
func (s *SummaryService) CompressIfNeeded(ctx context.Context, conversationID string) error {
	//查看摘要功能是否启用
	if !s.cfg.SummaryEnabled {
		return nil
	}
	//统计消息总数，只有到一定阈值才能压缩
	count, err := s.store.CountMessages(ctx, conversationID)
	if err != nil {
		return err
	}
	if count < int64(s.cfg.SummaryStartTurns*2) {
		return nil
	}
	//加载所有消息来摘要生成
	messages, err := s.store.LoadRecentMessages(ctx, conversationID, int(count))
	if err != nil {
		return err
	}
	//构建对话文本
	var convText string
	for _, m := range messages {
		convText += fmt.Sprintf("%s: %s\n", m.Role, m.Content)
	}
	//调用LLM生成摘要（low temperature保证稳定性）
	prompt := fmt.Sprintf(summaryPrompt, convText, s.cfg.SummaryMaxChars)
	resp, err := s.llm.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, chat.WithTemperature(0.3))
	if err != nil {
		return err
	}
	//提取思考内容并截断
	summary := llmutil.ExtractThinkContent(resp.Content)
	summary = llmutil.TruncateString(summary, s.cfg.SummaryMaxChars)
	//保存到数据库
	if err := s.store.SaveSummary(ctx, conversationID, summary); err != nil {
		return err
	}
	slog.Info("conversation summarized", "conversationID", conversationID, "len", len(summary))
	return nil
}

// LoadLatestSummary 加载会话的最新摘要
func (s *SummaryService) LoadLatestSummary(ctx context.Context, conversationID string) (string, error) {
	return s.store.LoadLatestSummary(ctx, conversationID)
}
