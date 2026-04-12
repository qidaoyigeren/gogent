package entity

// MessageFeedbackDO maps to t_message_feedback table.
type MessageFeedbackDO struct {
	BaseModel
	MessageID      string `gorm:"column:message_id;size:20;uniqueIndex:uk_msg_user" json:"messageId"`
	ConversationID string `gorm:"column:conversation_id;size:20;index" json:"conversationId"`
	UserID         string `gorm:"column:user_id;size:20;index" json:"userId"`
	Vote           int    `gorm:"column:vote;type:smallint" json:"vote"`
	Reason         string `gorm:"column:reason;size:255" json:"reason,omitempty"`
	Comment        string `gorm:"column:comment;size:1024" json:"comment,omitempty"`
}

func (MessageFeedbackDO) TableName() string { return "t_message_feedback" }
