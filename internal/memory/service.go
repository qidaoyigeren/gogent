package memory

import (
	"context"
	"gogent/internal/auth"
	"gogent/internal/chat"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"
)

type ConversationMemoryService struct {
	store      *Store          // 存储层
	summarySvc *SummaryService // 摘要服务
	keepTurns  int             // 保留的历史轮次
}

func NewConversationMemoryService(store *Store, summarySvc *SummaryService, keepTurns int) *ConversationMemoryService {
	return &ConversationMemoryService{
		store:      store,
		summarySvc: summarySvc,
		keepTurns:  keepTurns,
	}
}

// MemoryContext 会话记忆上下文
// 核心职责：
// 1. Summary: 压缩的老对话（摘要）
// 2. History: 最近 N 轮对话（未压缩）
type MemoryContext struct {
	Summary string         // 对话摘要（LLM 生成）
	History []chat.Message // 最近对话历史
}

// Load 并行加载会话记忆（摘要 + 最近历史）
// 核心职责：
// 1. 并行加载摘要和最近历史（errgroup）
// 2. 容错降级（加载失败不阻塞）
// 3. 历史规范化（确保以 USER 开头）
//
// 工作流程：
// 1. 启动两个 goroutine 并行加载
// 2. 等待两个任务完成
// 3. 返回 MemoryContext
//
// 注意：摘要和历史加载失败是非致命错误，会降级处理
func (s *ConversationMemoryService) Load(ctx context.Context, conversationID string) (*MemoryContext, error) {
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	result := &MemoryContext{}

	// 并行任务 1：加载摘要
	g.Go(func() error {
		summary, err := s.summarySvc.LoadLatestSummary(gCtx, conversationID)
		if err != nil {
			slog.Warn("failed to load summary", "err", err) // 非致命错误
			return nil
		}
		mu.Lock()
		result.Summary = summary
		mu.Unlock()
		return nil
	})

	// 并行任务 2：加载最近历史
	g.Go(func() error {
		// keepTurns * 2：每轮 2 条消息（user + assistant）
		messages, err := s.store.LoadRecentMessages(gCtx, conversationID, s.keepTurns*2)
		if err != nil {
			slog.Warn("failed to load history", "err", err) // 非致命错误
			return nil
		}
		// 规范化：确保以 USER 开头
		normalized := normalizeHistory(messages)
		mu.Lock()
		result.History = normalized
		mu.Unlock()
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return result, nil
}

// Append 追加消息到会话，并触发相关操作
//
// 核心职责：
// 1. 保存消息到数据库
// 2. USER 消息：更新会话 last_time（用于排序）
// 3. ASSISTANT 消息：异步触发摘要压缩
//
// 对齐 Java：
// - USER 消息：JdbcConversationMemoryStore#append → conversationService.createOrUpdate
// - ASSISTANT 消息：异步摘要压缩（Redisson 锁替换为 goroutine + 背景上下文）
//
// 注意：ASSISTANT 消息的摘要压缩在后台 goroutine 执行，不阻塞主流程
func (s *ConversationMemoryService) Append(ctx context.Context, conversationID string, msg chat.Message) (string, error) {
	msgID, err := s.store.SaveMessage(ctx, conversationID, msg)
	if err != nil {
		return "", err
	}

	switch msg.Role {
	case "user":
		// USER 消息：更新会话 last_time（用于列表排序）
		userID := auth.GetUserID(ctx)
		title := truncateTitle(msg.Content, 50) // 截断标题（最多 50 字符）
		s.store.TouchConversation(ctx, conversationID, userID, title)

	case "assistant":
		// ASSISTANT 消息：异步触发摘要压缩
		// 使用背景上下文并保留用户信息，确保 SaveSummary 写入非空 user_id
		bgCtx := auth.WithUser(context.Background(), auth.GetUser(ctx))
		go func() {
			if err := s.summarySvc.CompressIfNeeded(bgCtx, conversationID); err != nil {
				slog.Warn("summary compression failed", "err", err) // 非致命错误
			}
		}()
	}

	return msgID, nil
}

// truncateTitle 截断会话标题（保留前 n 个字符）
// 用于从第一条用户消息生成会话标题
func truncateTitle(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// normalizeHistory 规范化对话历史
// 核心职责：
// 1. 过滤空消息（Content 为空）
// 2. 确保历史以 USER 消息开头（跳过开头的 ASSISTANT 消息）
// 3. 保持原始顺序
func normalizeHistory(messages []chat.Message) []chat.Message {
	var result []chat.Message
	started := false

	for _, msg := range messages {
		if msg.Content == "" {
			continue
		}
		if !started && msg.Role != "user" {
			continue
		}
		started = true
		result = append(result, msg)
	}

	return result
}
