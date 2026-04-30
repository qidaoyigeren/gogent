package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"gogent/internal/entity"
	"strings"
	"time"

	"gorm.io/gorm"
)

type ExecuteTaskRequest struct {
	PipelineID    string                 `json:"pipelineId"`
	Source        DocumentSource         `json:"source"`
	Metadata      map[string]interface{} `json:"metadata"`
	VectorSpaceID *VectorSpaceID         `json:"vectorSpaceId,omitempty"`
	RawBytes      []byte                 `json:"-"`
	MimeType      string                 `json:"mimeType,omitempty"`
	DocID         string                 `json:"docId,omitempty"`
	KBID          string                 `json:"kbId,omitempty"`
	CreatedBy     string                 `json:"createdBy,omitempty"`
}

type TaskResult struct {
	TaskID     string            `json:"taskId"`
	PipelineID string            `json:"pipelineId"`
	Status     IngestionStatus   `json:"status"`
	ChunkCount int               `json:"chunkCount"`
	Message    string            `json:"message"`
	Context    *IngestionContext `json:"-"`
}

type TaskService struct {
	db        *gorm.DB
	pipelines *PipelineService
	runtime   *Runtime
}

// NewTaskService 绑定数据库、pipeline 定义服务和 runtime 执行器。
// Handler 和文档调度最终都通过这个服务把“执行请求”落成一条任务记录。
func NewTaskService(db *gorm.DB, pipelines *PipelineService, runtime *Runtime) *TaskService {
	return &TaskService{db: db, pipelines: pipelines, runtime: runtime}
}

// Execute 创建并同步执行一个 ingestion task。
// 它适合管理端“创建任务后立即跑”的入口：先读取 pipeline 定义，再写任务主表，
// 然后运行 runtime，最后把节点日志和汇总 metadata 一起持久化。
func (s *TaskService) Execute(ctx context.Context, req ExecuteTaskRequest) (*TaskResult, error) {
	if strings.TrimSpace(req.PipelineID) == "" {
		return nil, fmt.Errorf("必须传流水线ID")
	}
	// pipeline 是任务执行的模板，包含节点顺序、节点配置和条件表达式。
	pipeline, err := s.pipelines.GetDefinition(ctx, req.PipelineID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	// 先创建 RUNNING 任务行，保证即使后续执行失败，也能在任务列表里追踪到实例。
	task := entity.IngestionTaskDO{
		BaseModel:      entity.BaseModel{ID: nextID()},
		PipelineID:     req.PipelineID,
		SourceType:     string(req.Source.Type),
		SourceLocation: req.Source.Location,
		SourceFileName: req.Source.FileName,
		Status:         string(IngestionStatusRunning),
		StartedAt:      &now,
		CreatedBy:      req.CreatedBy,
		UpdatedBy:      req.CreatedBy,
	}
	if err := s.db.WithContext(ctx).Create(&task).Error; err != nil {
		return nil, err
	}

	// IngestionContext 是节点之间共享的内存状态；RawBytes、MimeType、DocID 等
	// 由上层 Handler 注入，后续节点会逐步填充 RawText、Chunks、Metadata。
	ingestCtx := &IngestionContext{
		TaskID:        task.ID,
		PipelineID:    req.PipelineID,
		Source:        req.Source,
		VectorSpaceID: req.VectorSpaceID,
		RawBytes:      req.RawBytes,
		DocID:         req.DocID,
		KBID:          req.KBID,
		FileName:      req.Source.FileName,
		MimeType:      req.MimeType,
		Status:        IngestionStatusRunning,
	}
	resultCtx, execErr := s.runtime.Execute(ctx, pipeline, ingestCtx)
	if resultCtx == nil {
		resultCtx = ingestCtx // 极端情况 engine 未返回指针，仍用初始 context 落库
	}

	// runtime 返回后，将内存态折叠回任务主表：状态、chunk 数、错误、摘要日志。
	task.Status = string(resultCtx.Status)
	task.ChunkCount = len(resultCtx.Chunks)
	task.ErrorMessage = errorMessage(execErr)
	completedAt := time.Now()
	task.CompletedAt = &completedAt
	task.LogsJson = marshalJSONString(buildTaskLogsJSON(resultCtx.Logs))
	task.MetadataJson = marshalJSONString(buildTaskMetadata(resultCtx))
	if task.Status == "" {
		task.Status = string(IngestionStatusFailed)
	}
	// 主表只保存汇总字段和 JSON 摘要；逐节点明细由 persistNodeLogs 写子表。
	if err := s.db.WithContext(ctx).Model(&entity.IngestionTaskDO{}).Where("id = ?", task.ID).Updates(map[string]interface{}{
		"status":        task.Status,
		"chunk_count":   task.ChunkCount,
		"error_message": task.ErrorMessage,
		"completed_at":  task.CompletedAt,
		"logs_json":     task.LogsJson,
		"metadata_json": task.MetadataJson,
		"updated_by":    req.CreatedBy,
	}).Error; err != nil {
		return nil, err
	}
	if err := s.persistNodeLogs(ctx, task, resultCtx.Logs); err != nil {
		return nil, err
	}

	message := "OK"
	if execErr != nil {
		// 即使执行失败，前面的任务状态也已经落库；返回错误信息给 API 层展示。
		message = execErr.Error()
	}
	return &TaskResult{
		TaskID:     task.ID,
		PipelineID: task.PipelineID,
		Status:     resultCtx.Status,
		ChunkCount: len(resultCtx.Chunks),
		Message:    message,
		Context:    resultCtx,
	}, nil
}

// RunRuntime 是“只执行 runtime，不新建 task 主记录”的轻量入口。
// 文档上传分块模式会先在 handler 中管理文档状态和任务队列，再用这里复用
// pipeline 节点执行能力；pipelineID 为空时使用 Runtime 内置默认链路。
func (s *TaskService) RunRuntime(ctx context.Context, pipelineID string, ingestCtx *IngestionContext) (*IngestionContext, error) {
	if ingestCtx == nil {
		ingestCtx = &IngestionContext{}
	}
	if strings.TrimSpace(pipelineID) == "" {
		pipeline := s.runtime.DefaultPipelineDefinition()
		ingestCtx.PipelineID = pipeline.ID
		return s.runtime.Execute(ctx, pipeline, ingestCtx) // 与知识库快速分块默认链一致
	}
	ingestCtx.PipelineID = pipelineID
	pipeline, err := s.pipelines.GetDefinition(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	return s.runtime.Execute(ctx, pipeline, ingestCtx)
}

// persistNodeLogs 把 context 中的每个 NodeLog 写入任务节点表。
// 这些记录用于管理端查看每个节点耗时、状态、错误和输出摘要。
func (s *TaskService) persistNodeLogs(ctx context.Context, task entity.IngestionTaskDO, logs []NodeLog) error {
	for _, log := range logs {
		// 一行 NodeLog → 一行 t_ingestion_task_node；OutputJson 经 truncate 防爆库
		record := entity.IngestionTaskNodeDO{
			BaseModel:    entity.BaseModel{ID: nextID()},
			TaskID:       task.ID,
			PipelineID:   task.PipelineID,
			NodeID:       log.NodeID,
			NodeType:     normalizeNodeType(log.NodeType),
			NodeOrder:    log.NodeOrder,
			Status:       resolveNodeStatus(log),
			DurationMs:   log.DurationMs,
			Message:      log.Message,
			ErrorMessage: log.Error,
			OutputJson:   truncateOutputJSON(log.Output),
		}
		if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
			return err
		}
	}
	return nil
}

// resolveNodeStatus 将内存态 NodeLog 转成数据库中的节点状态。
// Skip 不是失败，因为条件未满足属于 pipeline 的正常分支行为。
func resolveNodeStatus(log NodeLog) string {
	if !log.Success {
		return "failed"
	}
	if strings.HasPrefix(log.Message, "Skipped:") {
		return "skipped"
	}
	return "success"
}

// truncateOutputJSON 限制节点输出日志体积，避免大文本或大 metadata 撑爆任务表。
func truncateOutputJSON(output map[string]interface{}) string {
	raw := marshalJSONString(output)
	if len(raw) <= 1024*1024 {
		return raw
	}
	suffix := `{"truncated":true}`
	return raw[:(1024*1024)-len(suffix)] + suffix // 硬截断并打标，防止单节点 output 撑满库
}

// buildTaskLogsJSON 生成任务主表中的轻量日志数组，便于列表或详情快速展示。
func buildTaskLogsJSON(logs []NodeLog) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(logs))
	for _, log := range logs {
		items = append(items, map[string]interface{}{
			"nodeId":     log.NodeID,
			"nodeType":   log.NodeType,
			"durationMs": log.DurationMs,
			"message":    log.Message,
			"success":    log.Success,
			"error":      log.Error,
		})
	}
	return items
}

// buildTaskMetadata 汇总任务级 metadata：保留节点写入的元数据，同时补充文件来源、
// chunk 数、LLM 生成的关键词/问题等可用于排障和运营统计的信息。
func buildTaskMetadata(ctx *IngestionContext) map[string]interface{} {
	metadata := cloneMetadata(ctx.Metadata)
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	if len(ctx.Keywords) > 0 {
		metadata["keywords"] = ctx.Keywords
	}
	if len(ctx.Questions) > 0 {
		metadata["questions"] = ctx.Questions
	}
	metadata["fileName"] = ctx.FileName
	metadata["sourceType"] = ctx.Source.Type
	metadata["sourceLocation"] = ctx.Source.Location
	metadata["chunkCount"] = len(ctx.Chunks)
	return metadata
}

// cloneMetadata 复制 map，避免构建任务摘要时意外修改 context 中的原始 metadata。
func cloneMetadata(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	output := make(map[string]interface{}, len(input))
	for k, v := range input {
		output[k] = v
	}
	return output
}

// marshalJSONString 是任务日志落库的统一 JSON 序列化入口；失败时返回空串，
// 避免因为某个不可序列化字段影响主流程状态落库。
func marshalJSONString(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "" // 序列化失败不反压主流程：任务状态仍要落库
	}
	return string(data)
}
