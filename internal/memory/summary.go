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

// summaryPrompt 摘要生成提示词模板
// 参数：
// 1. 对话历史文本
// 2. 最大字符数限制
const summaryPrompt = `请将以下对话历史压缩为一段简洁的摘要，保留关键信息和上下文：

对话历史：
%s

要求：
1. 摘要不超过%d个字符
2. 保留关键实体、数字和结论
3. 使用第三人称
4. 只返回摘要文本`

// CompressIfNeeded 检查是否需要压缩并执行摘要生成
// 工作流程：
// 1. 检查是否启用摘要功能
// 2. 统计消息数量
// 3. 判断是否达到触发阈值（SummaryStartTurns × 2）
// 4. 加载所有消息
// 5. 构建对话文本
// 6. 调用 LLM 生成摘要（temperature=0.3）
// 7. 提取思考内容并截断
// 8. 保存摘要到数据库
//
// 触发时机：Append 方法在追加 ASSISTANT 消息后异步调用
func (s *SummaryService) CompressIfNeeded(ctx context.Context, conversationID string) error {
	if !s.cfg.SummaryEnabled {
		return nil // 摘要功能未启用
	}

	// 统计消息总数
	count, err := s.store.CountMessages(ctx, conversationID)
	if err != nil {
		return err
	}

	// 只有超过阈值才压缩（例如：SummaryStartTurns=10，则需要 20 条消息）
	if count < int64(s.cfg.SummaryStartTurns*2) {
		return nil
	}

	// 加载所有消息用于摘要生成
	messages, err := s.store.LoadRecentMessages(ctx, conversationID, int(count))
	if err != nil {
		return err
	}

	// 构建对话文本（格式：role: content）
	var convText string
	for _, msg := range messages {
		convText += fmt.Sprintf("%s: %s\n", msg.Role, msg.Content)
	}

	// 调用 LLM 生成摘要（低温度保证稳定性）
	prompt := fmt.Sprintf(summaryPrompt, convText, s.cfg.SummaryMaxChars)
	resp, err := s.llm.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, chat.WithTemperature(0.3))

	if err != nil {
		return fmt.Errorf("summary LLM call failed: %w", err)
	}

	// 提取思考内容（支持 DeepThinking 模型）并截断
	summary := llmutil.ExtractThinkContent(resp.Content)
	summary = llmutil.TruncateString(summary, s.cfg.SummaryMaxChars)

	// 保存摘要到数据库
	if err := s.store.SaveSummary(ctx, conversationID, summary); err != nil {
		return err
	}

	slog.Info("conversation summarized", "conversationID", conversationID, "len", len(summary))
	return nil
}

// LoadLatestSummary 加载会话的最新摘要
// 委托给 store 层执行
// 如果无摘要，返回空字符串（非错误）
func (s *SummaryService) LoadLatestSummary(ctx context.Context, conversationID string) (string, error) {
	return s.store.LoadLatestSummary(ctx, conversationID)
}
