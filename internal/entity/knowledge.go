package entity

import "time"

// KnowledgeBaseDO maps to t_knowledge_base.
type KnowledgeBaseDO struct {
	BaseModel
	Name           string `gorm:"column:name;size:128" json:"name"`
	EmbeddingModel string `gorm:"column:embedding_model;size:64" json:"embeddingModel"`
	CollectionName string `gorm:"column:collection_name;size:64;uniqueIndex:uk_collection_name" json:"collectionName"`
	CreatedBy      string `gorm:"column:created_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy      string `gorm:"column:updated_by;size:20" json:"updatedBy,omitempty"`
}

func (KnowledgeBaseDO) TableName() string { return "t_knowledge_base" }

// KnowledgeDocumentDO maps to t_knowledge_document.
type KnowledgeDocumentDO struct {
	BaseModel
	KBID            string      `gorm:"column:kb_id;size:20;index" json:"kbId"`
	DocName         string      `gorm:"column:doc_name;size:256" json:"docName"`
	SourceType      string      `gorm:"column:source_type;size:16" json:"sourceType"`
	SourceLocation  string      `gorm:"column:source_location;size:1024" json:"sourceLocation,omitempty"`
	ScheduleEnabled int         `gorm:"column:schedule_enabled;default:0" json:"scheduleEnabled"`
	ScheduleCron    string      `gorm:"column:schedule_cron;size:64" json:"scheduleCron,omitempty"`
	Enabled         FlexEnabled `gorm:"column:enabled;default:1" json:"enabled"`
	ChunkCount      int         `gorm:"column:chunk_count;default:0" json:"chunkCount"`
	FileURL         string      `gorm:"column:file_url;size:1024" json:"fileUrl"`
	FileType        string      `gorm:"column:file_type;size:16" json:"fileType"`
	FileSize        int64       `gorm:"column:file_size" json:"fileSize,omitempty"`
	ProcessMode     string      `gorm:"column:process_mode;size:16;default:chunk" json:"processMode"`
	ChunkStrategy   string      `gorm:"column:chunk_strategy;size:32" json:"chunkStrategy,omitempty"`
	ChunkConfig     string      `gorm:"column:chunk_config;type:jsonb" json:"chunkConfig,omitempty"`
	PipelineID      string      `gorm:"column:pipeline_id;size:20" json:"pipelineId,omitempty"`
	Status          string      `gorm:"column:status;size:16;default:pending" json:"status"`
	CreatedBy       string      `gorm:"column:created_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy       string      `gorm:"column:updated_by;size:20" json:"updatedBy,omitempty"`
}

func (KnowledgeDocumentDO) TableName() string { return "t_knowledge_document" }

// KnowledgeChunkDO maps to t_knowledge_chunk.
type KnowledgeChunkDO struct {
	BaseModel
	KBID        string      `gorm:"column:kb_id;size:20;index" json:"kbId"`
	DocID       string      `gorm:"column:doc_id;size:20;index" json:"docId"`
	ChunkIndex  int         `gorm:"column:chunk_index" json:"chunkIndex"`
	Content     string      `gorm:"column:content;type:text" json:"content"`
	ContentHash string      `gorm:"column:content_hash;size:64" json:"contentHash,omitempty"`
	CharCount   int         `gorm:"column:char_count" json:"charCount,omitempty"`
	TokenCount  int         `gorm:"column:token_count" json:"tokenCount,omitempty"`
	Enabled     FlexEnabled `gorm:"column:enabled;default:1" json:"enabled"`
	CreatedBy   string      `gorm:"column:created_by;size:20" json:"createdBy,omitempty"`
	UpdatedBy   string      `gorm:"column:updated_by;size:20" json:"updatedBy,omitempty"`
}

func (KnowledgeChunkDO) TableName() string { return "t_knowledge_chunk" }

// KnowledgeDocumentScheduleDO maps to t_knowledge_document_schedule.
type KnowledgeDocumentScheduleDO struct {
	BaseModel
	DocID           string      `gorm:"column:doc_id;size:20;index;uniqueIndex:uk_doc_id" json:"docId"`
	KBID            string      `gorm:"column:kb_id;size:20" json:"kbId"`
	CronExpr        string      `gorm:"column:cron_expr;size:64" json:"cronExpr"`
	Enabled         FlexEnabled `gorm:"column:enabled;default:0" json:"enabled"`
	NextRunTime     *time.Time  `gorm:"column:next_run_time;index" json:"nextRunTime,omitempty"`
	LastRunTime     *time.Time  `gorm:"column:last_run_time" json:"lastRunTime,omitempty"`
	LastSuccessTime *time.Time  `gorm:"column:last_success_time" json:"lastSuccessTime,omitempty"`
	LastStatus      string      `gorm:"column:last_status;size:16" json:"lastStatus,omitempty"`
	LastError       string      `gorm:"column:last_error;size:512" json:"lastError,omitempty"`
	LastEtag        string      `gorm:"column:last_etag;size:256" json:"lastEtag,omitempty"`
	LastModified    string      `gorm:"column:last_modified;size:256" json:"lastModified,omitempty"`
	LastContentHash string      `gorm:"column:last_content_hash;size:128" json:"lastContentHash,omitempty"`
	LockOwner       string      `gorm:"column:lock_owner;size:128" json:"lockOwner,omitempty"`
	LockUntil       *time.Time  `gorm:"column:lock_until;index" json:"lockUntil,omitempty"`
}

func (KnowledgeDocumentScheduleDO) TableName() string { return "t_knowledge_document_schedule" }

// KnowledgeDocumentScheduleExecDO maps to t_knowledge_document_schedule_exec.
type KnowledgeDocumentScheduleExecDO struct {
	BaseModel
	ScheduleID   string     `gorm:"column:schedule_id;size:20;index" json:"scheduleId"`
	DocID        string     `gorm:"column:doc_id;size:20;index" json:"docId"`
	KBID         string     `gorm:"column:kb_id;size:20" json:"kbId"`
	Status       string     `gorm:"column:status;size:16" json:"status"`
	Message      string     `gorm:"column:message;size:512" json:"message,omitempty"`
	StartTime    *time.Time `gorm:"column:start_time" json:"startTime,omitempty"`
	EndTime      *time.Time `gorm:"column:end_time" json:"endTime,omitempty"`
	FileName     string     `gorm:"column:file_name;size:512" json:"fileName,omitempty"`
	FileSize     int64      `gorm:"column:file_size" json:"fileSize,omitempty"`
	ContentHash  string     `gorm:"column:content_hash;size:128" json:"contentHash,omitempty"`
	Etag         string     `gorm:"column:etag;size:256" json:"etag,omitempty"`
	LastModified string     `gorm:"column:last_modified;size:256" json:"lastModified,omitempty"`
}

func (KnowledgeDocumentScheduleExecDO) TableName() string {
	return "t_knowledge_document_schedule_exec"
}

// KnowledgeDocumentChunkLogDO maps to t_knowledge_document_chunk_log.
type KnowledgeDocumentChunkLogDO struct {
	BaseModel
	DocID           string     `gorm:"column:doc_id;size:20;index" json:"docId"`
	Status          string     `gorm:"column:status;size:16" json:"status"`
	ProcessMode     string     `gorm:"column:process_mode;size:16" json:"processMode,omitempty"`
	ChunkStrategy   string     `gorm:"column:chunk_strategy;size:16" json:"chunkStrategy,omitempty"`
	PipelineID      string     `gorm:"column:pipeline_id;size:20" json:"pipelineId,omitempty"`
	ExtractDuration int64      `gorm:"column:extract_duration" json:"extractDuration,omitempty"`
	ChunkDuration   int64      `gorm:"column:chunk_duration" json:"chunkDuration,omitempty"`
	EmbedDuration   int64      `gorm:"column:embed_duration" json:"embedDuration,omitempty"`
	PersistDuration int64      `gorm:"column:persist_duration" json:"persistDuration,omitempty"`
	TotalDuration   int64      `gorm:"column:total_duration" json:"totalDuration,omitempty"`
	ChunkCount      int        `gorm:"column:chunk_count" json:"chunkCount,omitempty"`
	ErrorMessage    string     `gorm:"column:error_message;type:text" json:"errorMessage,omitempty"`
	StartTime       *time.Time `gorm:"column:start_time" json:"startTime,omitempty"`
	EndTime         *time.Time `gorm:"column:end_time" json:"endTime,omitempty"`
}

func (KnowledgeDocumentChunkLogDO) TableName() string { return "t_knowledge_document_chunk_log" }
