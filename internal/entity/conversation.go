package entity

import "time"

// ConversationDO maps to t_conversation table.
type ConversationDO struct {
	BaseModel
	ConversationID string     `gorm:"column:conversation_id;size:20;uniqueIndex:uk_conversation_user;index" json:"conversationId"`
	UserID         string     `gorm:"column:user_id;size:20;uniqueIndex:uk_conversation_user" json:"userId"`
	Title          string     `gorm:"column:title;size:128" json:"title"`
	LastTime       *time.Time `gorm:"column:last_time" json:"lastTime,omitempty"`
}

func (ConversationDO) TableName() string { return "t_conversation" }

// ConversationMessageDO maps to t_message table.
type ConversationMessageDO struct {
	ID             string    `gorm:"column:id;primaryKey" json:"id"`
	ConversationID string    `gorm:"column:conversation_id;size:20;index:idx_conversation_user_time" json:"conversationId"`
	UserID         string    `gorm:"column:user_id;size:20;index:idx_conversation_user_time" json:"userId"`
	Role           string    `gorm:"column:role;size:16" json:"role"`
	Content        string    `gorm:"column:content;type:text" json:"content"`
	CreateTime     time.Time `gorm:"column:create_time;autoCreateTime;index:idx_conversation_user_time" json:"createTime"`
	UpdateTime     time.Time `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
	Deleted        int       `gorm:"column:deleted;default:0" json:"-"`
}

func (ConversationMessageDO) TableName() string { return "t_message" }

// ConversationSummaryDO maps to t_conversation_summary table.
type ConversationSummaryDO struct {
	ID             string    `gorm:"column:id;primaryKey" json:"id"`
	ConversationID string    `gorm:"column:conversation_id;size:20;index:idx_conv_user" json:"conversationId"`
	UserID         string    `gorm:"column:user_id;size:20;index:idx_conv_user" json:"userId"`
	LastMessageID  string    `gorm:"column:last_message_id;size:20" json:"lastMessageId"`
	Content        string    `gorm:"column:content;type:text" json:"content"`
	CreateTime     time.Time `gorm:"column:create_time;autoCreateTime" json:"createTime"`
	UpdateTime     time.Time `gorm:"column:update_time;autoUpdateTime" json:"updateTime"`
	Deleted        int       `gorm:"column:deleted;default:0" json:"-"`
}

func (ConversationSummaryDO) TableName() string { return "t_conversation_summary" }
