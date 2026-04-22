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

type MemoryContext struct {
	Summary string         // 对话摘要（LLM 生成）
	History []chat.Message // 最近对话历史
}

// Load 并行加载会话记忆（摘要 + 最近历史）
// 核心职责：
// 1. 并行加载摘要和最近历史（errgroup）
// 2. 容错降级（加载失败不阻塞）
// 3. 历史规范化（确保以 USER 开头）
func (s *ConversationMemoryService) Load(ctx context.Context, conversationID string) (*MemoryContext, error) {
	g, gCtx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	result := &MemoryContext{}
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
	g.Go(func() error {
		history, err := s.store.LoadRecentMessages(gCtx, conversationID, s.keepTurns*2)
		if err != nil {
			slog.Warn("failed to load history", "err", err) // 非致命错误
			return nil
		}
		mu.Lock()
		result.History = normalizeHistory(history)
		mu.Unlock()
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return result, nil
}

func normalizeHistory(history []chat.Message) []chat.Message {
	var result []chat.Message
	started := false

	for _, msg := range history {
		if msg.Content == "" {
			continue
		}
		if msg.Role != "user" && !started {
			continue
		}
		started = true
		result = append(result, msg)
	}
	return result
}

// Append 追加消息到会话，并触发相关操作
//
// 核心职责：
// 1. 保存消息到数据库
// 2. USER 消息：更新会话 last_time（用于排序）
// 3. ASSISTANT 消息：异步触发摘要压缩
func (s *ConversationMemoryService) Append(ctx context.Context, conversationID string, msg chat.Message) (string, error) {
	msgID, err := s.store.SaveMessage(ctx, conversationID, msg)
	if err != nil {
		return "", err
	}
	switch msg.Role {
	case "user":
		userID := auth.GetUserID(ctx)
		title := truncateTitle(msg.Content, 50)
		s.store.TouchConversation(ctx, conversationID, userID, title)
	case "assistant":
		bgCtx := auth.WithUser(context.Background(), auth.GetUser(ctx))
		go func() {
			if err := s.summarySvc.CompressIfNeeded(bgCtx, conversationID); err != nil {
				slog.Warn("failed to compress summary", "err", err)
			}
		}()
	}
	return msgID, nil
}

func truncateTitle(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
