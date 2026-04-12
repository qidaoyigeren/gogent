package entity

import "time"

// RagTraceRunDO maps to t_rag_trace_run table.
type RagTraceRunDO struct {
	BaseModel
	TraceID        string     `gorm:"column:trace_id;size:64;uniqueIndex:uk_run_id" json:"traceId"`
	TraceName      string     `gorm:"column:trace_name;size:128" json:"traceName,omitempty"`
	EntryMethod    string     `gorm:"column:entry_method;size:256" json:"entryMethod,omitempty"`
	ConversationID string     `gorm:"column:conversation_id;size:20;index" json:"conversationId,omitempty"`
	TaskID         string     `gorm:"column:task_id;size:20;index" json:"taskId,omitempty"`
	UserID         string     `gorm:"column:user_id;size:20;index" json:"userId,omitempty"`
	Status         string     `gorm:"column:status;size:16;default:RUNNING" json:"status"`
	ErrorMessage   string     `gorm:"column:error_message;size:1000" json:"errorMessage,omitempty"`
	StartTime      *time.Time `gorm:"column:start_time" json:"startTime"`
	EndTime        *time.Time `gorm:"column:end_time" json:"endTime,omitempty"`
	DurationMs     int64      `gorm:"column:duration_ms" json:"durationMs,omitempty"`
	ExtraData      string     `gorm:"column:extra_data;type:text" json:"extraData,omitempty"`
}

func (RagTraceRunDO) TableName() string { return "t_rag_trace_run" }

// RagTraceNodeDO maps to t_rag_trace_node table.
type RagTraceNodeDO struct {
	BaseModel
	TraceID      string     `gorm:"column:trace_id;size:20;index;uniqueIndex:uk_run_node" json:"traceId"`
	NodeID       string     `gorm:"column:node_id;size:20;uniqueIndex:uk_run_node" json:"nodeId"`
	ParentNodeID string     `gorm:"column:parent_node_id;size:20" json:"parentNodeId,omitempty"`
	Depth        int        `gorm:"column:depth;default:0" json:"depth"`
	NodeType     string     `gorm:"column:node_type;size:16" json:"nodeType,omitempty"`
	NodeName     string     `gorm:"column:node_name;size:128" json:"nodeName"`
	ClassName    string     `gorm:"column:class_name;size:256" json:"className,omitempty"`
	MethodName   string     `gorm:"column:method_name;size:128" json:"methodName,omitempty"`
	Status       string     `gorm:"column:status;size:16;default:RUNNING" json:"status"`
	ErrorMessage string     `gorm:"column:error_message;size:1000" json:"errorMessage,omitempty"`
	StartTime    *time.Time `gorm:"column:start_time" json:"startTime"`
	EndTime      *time.Time `gorm:"column:end_time" json:"endTime,omitempty"`
	DurationMs   int64      `gorm:"column:duration_ms" json:"durationMs,omitempty"`
	ExtraData    string     `gorm:"column:extra_data;type:text" json:"extraData,omitempty"`
}

func (RagTraceNodeDO) TableName() string { return "t_rag_trace_node" }
