package service

import (
	"context"
	"gogent/internal/chat"
	"gogent/internal/entity"
	"gogent/pkg/idgen"
	"log/slog"
	"strings"

	"gorm.io/gorm"
)

// ========================= 典型调用点 =========================
//会话首屏辅助方法 —— 初始化会话、标题生成、消息落库。

// FindOrCreateConversation 以 (conversationID, 软删过滤) 查会话；不存在则按 userID 建一条。
func FindOrCreateConversation(db *gorm.DB, conversationID, userID string) *entity.ConversationDO {
	if db == nil {
		return nil
	}
	var conv entity.ConversationDO
	// deleted=0 过滤软删；一个 conversationID 全局唯一
	err := db.Where("conversation_id = ? AND deleted = 0", conversationID).First(&conv).Error
	if err == nil {
		return &conv
	}
	// 找不到走新建：Title 留空表示“待 LLM 生成”
	conv = entity.ConversationDO{
		BaseModel:      entity.BaseModel{ID: idgen.NextIDStr()},
		ConversationID: conversationID,
		UserID:         userID,
		Title:          "",
	}
	db.Create(&conv)
	return &conv
}

// ResolveTitle 读取会话标题；若为空则立即把 defaultTitle 写回并返回。
// 用途：前端 finish/cancel 事件需要 Title，确保首次拉取不会显示空字符串。
// 写入使用 UPDATE（不是整行替换），避免覆盖其他字段。
func ResolveTitle(db *gorm.DB, conversationID, defaultTitle string) string {
	if db == nil {
		return defaultTitle
	}
	var conv entity.ConversationDO
	if err := db.Where("conversation_id = ? AND deleted = 0", conversationID).First(&conv).Error; err != nil {
		return defaultTitle
	}
	if conv.Title != "" {
		return conv.Title
	}
	// 首次访问落默认标题，防止下次仍为空
	db.Model(&entity.ConversationDO{}).Where("conversation_id = ? AND deleted = 0", conversationID).Update("title", defaultTitle)
	return defaultTitle
}

// GenerateTitleAsync 异步请求 LLM 生成简洁标题并回写 DB。
// 为什么异步：首屏要立刻向前端 meta/reject/finish，标题生成耗时不能阻塞 SSE。
// 写入条件 "title = ” OR title IS NULL"：避免覆盖用户手工重命名的标题（竞态保护）。
func GenerateTitleAsync(db *gorm.DB, llm chat.LLMService, conversationID, question string) {
	if db == nil || llm == nil || question == "" {
		return
	}

	go func() {
		// 使用独立 ctx：即便主请求 ctx 已 cancel，标题生成仍可继续（best-effort）
		ctx := context.Background()
		messages := []chat.Message{
			{Role: "system", Content: "你是一个会话标题生成器。根据用户的第一个问题，生成一个简洁的会话标题（不超过20个字）。只输出标题文字，不要加引号或其他格式。"},
			{Role: "user", Content: question},
		}

		// 较高温度 0.7 让标题多样；max_tokens=50 足够 20 字以内
		resp, err := llm.Chat(ctx, messages, chat.WithTemperature(0.7), chat.WithMaxTokens(50))
		if err != nil {
			slog.Warn("title generation failed", "err", err)
			return
		}

		title := strings.TrimSpace(resp.Content)
		if title == "" {
			return
		}
		// 按 rune 截断 50（DB 字段通常 varchar(100)）：CJK 安全
		runes := []rune(title)
		if len(runes) > 50 {
			title = string(runes[:50])
		}

		// 只在标题仍为空时才更新，避免覆盖用户改过的标题
		db.Model(&entity.ConversationDO{}).Where("conversation_id = ? AND (title = '' OR title IS NULL) AND deleted = 0", conversationID).Update("title", title)
		slog.Info("generated conversation title", "conversationID", conversationID, "title", title)
	}()
}

// SaveConversationMessage 把一条消息写入 t_conversation_message。
// 返回消息主键 ID（雪花算法），调用方把它回填给前端用于反馈接口定位。
// 主要用途：memory 服务未启用或写入失败时的兜底路径（chat_handler 的限流拒答、guidance 等场景）。
func SaveConversationMessage(db *gorm.DB, conversationID, userID, role, content string) string {
	if db == nil {
		return ""
	}
	id := idgen.NextIDStr()
	msg := entity.ConversationMessageDO{
		ID:             id,
		ConversationID: conversationID,
		UserID:         userID,
		Role:           role, // "user" / "assistant" / "system"
		Content:        content,
	}
	db.Create(&msg)
	return id
}
