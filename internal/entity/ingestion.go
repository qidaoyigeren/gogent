package entity

import "time"

// IngestionPipelineDO maps to t_ingestion_pipeline.
type IngestionPipelineDO struct {
	BaseModel
	Name        string `gorm:"column:name;size:100;uniqueIndex:uk_ingestion_pipeline_name" json:"name"`
	Description string `gorm:"column:description;type:text" json:"description,omitempty"`
	CreatedBy   string `gorm:"column:created_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy   string `gorm:"column:updated_by;size:20" json:"updatedBy,omitempty"`
}

func (IngestionPipelineDO) TableName() string { return "t_ingestion_pipeline" }

// IngestionPipelineNodeDO maps to t_ingestion_pipeline_node.
type IngestionPipelineNodeDO struct {
	BaseModel
	PipelineID    string `gorm:"column:pipeline_id;size:20;index;uniqueIndex:uk_ingestion_pipeline_node" json:"pipelineId"`
	NodeID        string `gorm:"column:node_id;size:20;uniqueIndex:uk_ingestion_pipeline_node" json:"nodeId"`
	NodeType      string `gorm:"column:node_type;size:16" json:"nodeType"`
	NextNodeID    string `gorm:"column:next_node_id;size:20" json:"nextNodeId,omitempty"`
	SettingsJson  string `gorm:"column:settings_json;type:jsonb" json:"settingsJson,omitempty"`
	ConditionJson string `gorm:"column:condition_json;type:jsonb" json:"conditionJson,omitempty"`
}

func (IngestionPipelineNodeDO) TableName() string { return "t_ingestion_pipeline_node" }

// IngestionTaskDO maps to t_ingestion_task.
type IngestionTaskDO struct {
	BaseModel
	PipelineID     string     `gorm:"column:pipeline_id;size:20;index" json:"pipelineId"`
	SourceType     string     `gorm:"column:source_type;size:20" json:"sourceType"`
	SourceLocation string     `gorm:"column:source_location;type:text" json:"sourceLocation,omitempty"`
	SourceFileName string     `gorm:"column:source_file_name;size:255" json:"sourceFileName,omitempty"`
	Status         string     `gorm:"column:status;size:16" json:"status"`
	ChunkCount     int        `gorm:"column:chunk_count;default:0" json:"chunkCount,omitempty"`
	ErrorMessage   string     `gorm:"column:error_message;type:text" json:"errorMessage,omitempty"`
	LogsJson       string     `gorm:"column:logs_json;type:jsonb" json:"logsJson,omitempty"`
	MetadataJson   string     `gorm:"column:metadata_json;type:jsonb" json:"metadataJson,omitempty"`
	StartedAt      *time.Time `gorm:"column:started_at" json:"startedAt,omitempty"`
	CompletedAt    *time.Time `gorm:"column:completed_at" json:"completedAt,omitempty"`
	CreatedBy      string     `gorm:"column:created_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy      string     `gorm:"column:updated_by;size:20" json:"updatedBy,omitempty"`
}

func (IngestionTaskDO) TableName() string { return "t_ingestion_task" }

// IngestionTaskNodeDO maps to t_ingestion_task_node.
type IngestionTaskNodeDO struct {
	BaseModel
	TaskID       string `gorm:"column:task_id;size:20;index" json:"taskId"`
	PipelineID   string `gorm:"column:pipeline_id;size:20;index" json:"pipelineId"`
	NodeID       string `gorm:"column:node_id;size:20" json:"nodeId"`
	NodeType     string `gorm:"column:node_type;size:16" json:"nodeType"`
	NodeOrder    int    `gorm:"column:node_order;default:0" json:"nodeOrder"`
	Status       string `gorm:"column:status;size:16" json:"status"`
	DurationMs   int64  `gorm:"column:duration_ms;default:0" json:"durationMs"`
	Message      string `gorm:"column:message;type:text" json:"message,omitempty"`
	ErrorMessage string `gorm:"column:error_message;type:text" json:"errorMessage,omitempty"`
	OutputJson   string `gorm:"column:output_json;type:text" json:"outputJson,omitempty"`
}

func (IngestionTaskNodeDO) TableName() string { return "t_ingestion_task_node" }
