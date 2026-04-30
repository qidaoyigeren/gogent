package guidance

import (
	"sync"
	"time"
)

// AmbiguityPromptPrefix is the start of the assistant disambiguation message (must match service.go).
const AmbiguityPromptPrefix = "您的问题可能涉及多个方面"

// ConversationPending 会话级歧义选项待处理状态
// 核心职责：
// 1. 存储每个会话的歧义选项节点 ID
// 2. 等待用户选择数字（1..n）
// 3. 15 分钟 TTL 自动过期
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

// Set 存储待处理选项节点 ID
// TTL 默认 15 分钟
func (p *ConversationPending) Set(conversationID string, nodeIDs []string) {
	// 参数验证
	if conversationID == "" || len(nodeIDs) == 0 {
		return
	}
	// 复制切片（避免外部修改）
	cp := append([]string(nil), nodeIDs...)
	p.mu.Lock()
	defer p.mu.Unlock()
	// 存储并设置 15 分钟过期
	p.m[conversationID] = pendingEntry{ids: cp, exp: time.Now().Add(15 * time.Minute)}
}

// Peek 查看待处理选项（不移除）
// 如果不存在或已过期，返回 nil 并清理
func (p *ConversationPending) Peek(conversationID string) []string {
	// 参数验证
	if conversationID == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[conversationID]
	// 检查是否存在且未过期
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(p.m, conversationID) // 清理过期条目
		}
		return nil
	}
	// 返回副本（避免外部修改）
	out := append([]string(nil), e.ids...)
	return out
}

// Clear 清除会话的待处理状态
// 调用时机：用户成功选择或放弃选择后
func (p *ConversationPending) Clear(conversationID string) {
	if conversationID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, conversationID)
}
