package service

import (
	"gogent/internal/entity"
	"time"

	"gorm.io/gorm"
)

// ========================= 定位 =========================
//会话分组查询服务
// memory 摘要、滑窗载入、反馈等多个上层模块都需要对 t_conversation_message 做分组查询。
// 把这些只读查询集中在这里，避免在 handler/memory 里重复写 where 条件。

type ConversationGroupService struct {
	db *gorm.DB
}

func NewConversationGroupService(db *gorm.DB) *ConversationGroupService {
	return &ConversationGroupService{db: db}
}

// ListLatestUserOnlyMessages 返回指定会话最新 limit 条 user 消息，按 create_time DESC 排序。
// 为什么只取 user：部分场景（问题重写、摘要、引导）只需要用户侧历史文本作为上下文种子。
// 参数校验：空 ID / limit<=0 都返回空切片，不抛错。
func (s *ConversationGroupService) ListLatestUserOnlyMessages(conversationID, userID string, limit int) []entity.ConversationMessageDO {
	if conversationID == "" || userID == "" || limit <= 0 {
		return []entity.ConversationMessageDO{}
	}

	var messages []entity.ConversationMessageDO
	// 强制带上 userID 过滤，既防越权也能利用 (conversation_id, user_id) 的复合索引
	s.db.Where("conversation_id = ? AND user_id = ? AND role = ? AND deleted = 0",
		conversationID, userID, "user").
		Order("create_time DESC").
		Limit(limit).
		Find(&messages)

	return messages
}

// ListMessagesBetweenIds 返回 (afterID, beforeID) 之间的消息（不含端点），只保留 user + assistant。
// 典型用途：memory 摘要恢复时，取上次摘要到“当前最大消息 ID”的增量片段继续汇总。
// 排序：id ASC（雪花 ID 随时间单调递增，等价于 create_time ASC 但更稳定）。
func (s *ConversationGroupService) ListMessagesBetweenIds(conversationID, userID, afterID, beforeID string) []entity.ConversationMessageDO {
	if conversationID == "" || userID == "" {
		return []entity.ConversationMessageDO{}
	}

	query := s.db.Where("conversation_id = ? AND user_id = ? AND role IN ? AND deleted = 0",
		conversationID, userID, []string{"user", "assistant"})

	// 任一边界为空则不加条件（开区间端点可选）
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	if beforeID != "" {
		query = query.Where("id < ?", beforeID)
	}

	var messages []entity.ConversationMessageDO
	query.Order("id ASC").Find(&messages)

	return messages
}

// FindMaxMessageIdAtOrBefore 找出 at 时刻及之前、该会话的最大消息 ID。
// 用于摘要记忆：记录“摘要覆盖到 messageID X”，后续只需读取 (X, 当前] 之间的增量。
// 返回空串表示未找到（空会话或 at 早于所有消息）。
func (s *ConversationGroupService) FindMaxMessageIdAtOrBefore(conversationID, userID string, at time.Time) string {
	if conversationID == "" || userID == "" || at.IsZero() {
		return ""
	}

	var msg entity.ConversationMessageDO
	err := s.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0 AND create_time <= ?",
		conversationID, userID, at).
		Order("id DESC"). // ID 单调递增，取 DESC 首条即最大
		First(&msg).Error

	if err != nil {
		return ""
	}
	return msg.ID
}

// CountUserMessages 统计会话内 user 消息数量。
// 用于判断是否到达“开启摘要”阈值（summaryStartTurns）。
func (s *ConversationGroupService) CountUserMessages(conversationID, userID string) int64 {
	if conversationID == "" || userID == "" {
		return 0
	}

	var count int64
	s.db.Model(&entity.ConversationMessageDO{}).
		Where("conversation_id = ? AND user_id = ? AND role = ? AND deleted = 0",
			conversationID, userID, "user").
		Count(&count)

	return count
}

// FindLatestSummary 返回该会话最新的一条摘要（按 id DESC）。
// 返回 nil 表示从未生成过摘要（首次对话或未启用摘要）。
func (s *ConversationGroupService) FindLatestSummary(conversationID, userID string) *entity.ConversationSummaryDO {
	if conversationID == "" || userID == "" {
		return nil
	}

	var summary entity.ConversationSummaryDO
	err := s.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0",
		conversationID, userID).
		Order("id DESC").
		First(&summary).Error

	if err != nil {
		return nil
	}
	return &summary
}

// FindConversation 按 (conversationID, userID) 点查一条会话。
// 带 userID 条件是防越权的关键：A 用户无法读取/修改 B 用户的 conversationID。
// 返回 nil 表示无权访问或不存在。
func (s *ConversationGroupService) FindConversation(conversationID, userID string) *entity.ConversationDO {
	if conversationID == "" || userID == "" {
		return nil
	}

	var conv entity.ConversationDO
	err := s.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0",
		conversationID, userID).
		First(&conv).Error

	if err != nil {
		return nil
	}
	return &conv
}

// FindOrCreateConversation 与包级同名函数相似，但：
//   - 此方法不做默认 Title 填充（留给外层决策，例如 chat_handler 的 fallbackTitle）
//   - 带 userID 过滤，严格归属
//
// 返回 nil 表示创建失败（例如 DB 异常）。
func (s *ConversationGroupService) FindOrCreateConversation(conversationID, userID string) *entity.ConversationDO {
	conv := s.FindConversation(conversationID, userID)
	if conv != nil {
		return conv
	}

	// 不存在则新建；Title/BaseModel.ID 交给 GORM 钩子或 DB 默认值
	conv = &entity.ConversationDO{
		ConversationID: conversationID,
		UserID:         userID,
	}
	if err := s.db.Create(conv).Error; err != nil {
		return nil
	}
	return conv
}
