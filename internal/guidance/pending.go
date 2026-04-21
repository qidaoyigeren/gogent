package guidance

import (
	"sync"
	"time"
)

const AmbiguityPromptPrefix = "您的问题可能涉及多个方面"

// ConversationPending 会话级歧义选项待处理状态
// 注意：仅进程内存储（单实例），多实例需改用 Redis
type ConversationPending struct {
	mu sync.Mutex
	m  map[string]pendingEntry // conversationID -> 待处理选项
}

type pendingEntry struct {
	ids []string  // 选项节点 ID 列表（与数字 1..n 平行）
	exp time.Time // 过期时间
}

func NewConversationPending() *ConversationPending {
	return &ConversationPending{m: make(map[string]pendingEntry)}
}

func (p *ConversationPending) Set(conversationID string, nodeIDs []string) {
	if conversationID == "" || len(nodeIDs) == 0 {
		return
	}
	cp := append([]string(nil), nodeIDs...)
	p.mu.Lock()
	defer p.mu.Unlock()

	p.m[conversationID] = pendingEntry{
		ids: cp,
		exp: time.Now().Add(15 * time.Minute),
	}
}

// Peek 查看待处理选项（不移除）
func (p *ConversationPending) Peek(conversationID string) []string {
	if conversationID == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[conversationID]
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(p.m, conversationID) // 清理过期条目
		}
		return nil
	}
	out := append([]string(nil), e.ids...)
	return out
}

func (p *ConversationPending) Clear(conversationID string) {
	if conversationID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, conversationID)
}
