package memory

import (
	"context"
	"gogent/internal/auth"
	"gogent/internal/chat"
	"gogent/pkg/idgen"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MessageRecord 消息记录结构体，映射到 t_message 表
type MessageRecord struct {
	ID             string    `gorm:"column:id;primaryKey;size:20"`
	ConversationID string    `gorm:"column:conversation_id;size:20;index"`
	UserID         string    `gorm:"column:user_id;size:20"`
	Role           string    `gorm:"column:role;size:16"`      // 消息角色：user/assistant
	Content        string    `gorm:"column:content;type:text"` // 消息内容
	CreateTime     time.Time `gorm:"column:create_time;autoCreateTime"`
	UpdateTime     time.Time `gorm:"column:update_time;autoUpdateTime"`
	Deleted        int       `gorm:"column:deleted;default:0"` // 软删除标记：0=未删除，1=已删除
}

func (MessageRecord) TableName() string { return "t_message" }

// SummaryRecord 对话摘要记录结构体，映射到 t_conversation_summary 表
type SummaryRecord struct {
	ID             string    `gorm:"column:id;primaryKey;size:20"`
	ConversationID string    `gorm:"column:conversation_id;size:20;index"`
	UserID         string    `gorm:"column:user_id;size:20"`
	LastMessageID  string    `gorm:"column:last_message_id;size:20"` // 摘要覆盖的最后一条消息 ID
	Content        string    `gorm:"column:content;type:text"`       // 摘要内容
	CreateTime     time.Time `gorm:"column:create_time;autoCreateTime"`
	UpdateTime     time.Time `gorm:"column:update_time;autoUpdateTime"`
	Deleted        int       `gorm:"column:deleted;default:0"`
}

func (SummaryRecord) TableName() string { return "t_conversation_summary" }

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// conversationRow 会话行结构体，用于更新 t_conversation 表
type conversationRow struct {
	ID             string     `gorm:"column:id;primaryKey"`
	ConversationID string     `gorm:"column:conversation_id"`
	UserID         string     `gorm:"column:user_id"`
	Title          string     `gorm:"column:title"`     // 会话标题
	LastTime       *time.Time `gorm:"column:last_time"` // 最后活跃时间（用于排序）
	Deleted        int        `gorm:"column:deleted;default:0"`
	CreateTime     time.Time  `gorm:"column:create_time;autoCreateTime"`
	UpdateTime     time.Time  `gorm:"column:update_time;autoUpdateTime"`
}

func (conversationRow) TableName() string { return "t_conversation" }

// TouchConversation 更新会话的 last_time 字段（UPSERT 操作）
func (s *Store) TouchConversation(ctx context.Context, conversationID, userID, title string) {
	now := time.Now()
	row := conversationRow{
		ID:             idgen.NextIDStr(),
		ConversationID: conversationID,
		UserID:         userID,
		Title:          title,
		LastTime:       &now,
	}
	s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "conversation_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]interface{}{"last_time": now}),
		}).
		Create(&row)
}

// SaveMessage 持久化聊天消息，返回生成的消息 ID
func (s *Store) SaveMessage(ctx context.Context, conversationID string, msg chat.Message) (string, error) {
	id := idgen.NextIDStr()
	record := MessageRecord{
		ID:             id,
		ConversationID: conversationID,
		UserID:         auth.GetUserID(ctx),
		Role:           msg.Role,
		Content:        msg.Content,
	}
	return id, s.db.WithContext(ctx).Create(&record).Error
}

// LoadRecentMessages 加载会话的最近 N 条消息
func (s *Store) LoadRecentMessages(ctx context.Context, conversationID string, limit int) ([]chat.Message, error) {
	var records []MessageRecord
	err := s.db.WithContext(ctx).
		Where("conversation_id = ? AND deleted = 0", conversationID).
		Order("create_time DESC").
		Limit(limit).
		Find(&records).Error
	if err != nil {
		return nil, err
	}

	// 反转为正序（数据库是倒序查询）
	messages := make([]chat.Message, len(records))
	for i, r := range records {
		messages[len(records)-1-i] = chat.Message{
			Role:    r.Role,
			Content: r.Content,
		}
	}
	return messages, nil
}

// CountMessages 统计会话的消息总数（未删除）
func (s *Store) CountMessages(ctx context.Context, conversationID string) (int64, error) {
	var count int64
	err := s.db.WithContext(ctx).
		Model(&MessageRecord{}).
		Where("conversation_id = ? AND deleted = 0", conversationID).
		Count(&count).Error
	return count, err
}

// SaveSummary 持久化对话摘要
func (s *Store) SaveSummary(ctx context.Context, conversationID string, summary string) error {
	record := SummaryRecord{
		ID:             idgen.NextIDStr(),
		ConversationID: conversationID,
		UserID:         auth.GetUserID(ctx),
		Content:        summary,
	}
	return s.db.WithContext(ctx).Create(&record).Error
}

// LoadLatestSummary 加载最新的对话摘要
func (s *Store) LoadLatestSummary(ctx context.Context, conversationID string) (string, error) {
	var record SummaryRecord
	err := s.db.WithContext(ctx).
		Where("conversation_id = ? AND deleted = 0", conversationID).
		Order("create_time DESC").
		First(&record).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", nil
		}
		return "", err
	}
	return record.Content, nil
}
