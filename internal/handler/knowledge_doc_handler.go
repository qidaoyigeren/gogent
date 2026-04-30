package handler

// Day14 学习注释：knowledge_doc_handler.go 是知识库文档离线入库链路的 HTTP 控制中枢。
// 它横跨了文件存储、runtime 执行、向量写入、调度管理和 token 统计，是 Day14 最核心的 Handler。
//
// 关键设计：
//   - 两种入库模式：
//       "chunk" 模式：内置轻量链路（ParserNode + ChunkerNode），适合普通文件上传
//       "pipeline" 模式：使用 ingestion.Runtime + 用户自定义流水线，SkipIndexerWrite=true
//         避免 indexer 重复写向量，由 processChunkPipeline 统一处理持久化
//   - 异步 worker 机制：
//       upload 只存文档行（pending），不直接执行分块
//       startChunk 把任务写入 ingestion_task 表
//       StartChunkTaskWorker/chunkTaskWorkerLoop 每 2 秒轮询 claim 一个 pending 任务
//       claim 使用"读候选 + 条件更新 RowsAffected 判断"避免多实例竞争
//   - 全量替换策略：
//       processChunkPipeline 执行前先删旧 chunk 和旧向量，再写新结果
//       ChunkDO.ContentHash 记录内容哈希，供运营统计查重（暂未去重，记录即用）
//   - 调度集成：
//       upload/update/enable 时通过 KnowledgeDocumentScheduleService.UpsertSchedule 同步 schedule
//       RefreshDocument 是调度器调用的入口，直接同步执行，不走队列
//
// 关键并发安全保证：
//   - 文档处于 running/pending 时，startChunk 会拦截重复触发
//   - chunk 增删改操作均检查 doc.Status == "running"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gogent/internal/auth"
	"gogent/internal/embedding"
	"gogent/internal/entity"
	"gogent/internal/ingestion"
	internalparser "gogent/internal/parser"
	"gogent/internal/scheduler"
	"gogent/internal/storage"
	"gogent/internal/token"
	"gogent/internal/vector"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// docPathID returns the trimmed :docId path parameter.
func docPathID(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("docId"))
	return id, id != ""
}

type KnowledgeDocHandler struct {
	db        *gorm.DB
	fileStore storage.FileStore
	tasks     *ingestion.TaskService
	vectorSvc vector.VectorStoreService
	embSvc    embedding.EmbeddingService
	tika      *internalparser.TikaParser
	schedules *scheduler.KnowledgeDocumentScheduleService
	counter   *token.HeuristicCounter
	queueStop chan struct{}
	queueWG   sync.WaitGroup
}

// knowledgeDocumentUploadRequest 对应 multipart 上传接口的表单字段。
// file 类型使用 multipart 文件；url 类型使用 SourceLocation，并可开启调度刷新。
type knowledgeDocumentUploadRequest struct {
	SourceType      string `form:"sourceType"`
	SourceLocation  string `form:"sourceLocation"`
	ScheduleEnabled *bool  `form:"scheduleEnabled"`
	ScheduleCron    string `form:"scheduleCron"`
	ProcessMode     string `form:"processMode"`
	ChunkStrategy   string `form:"chunkStrategy"`
	ChunkConfig     string `form:"chunkConfig"`
	PipelineID      string `form:"pipelineId"`
}

// knowledgeDocumentUpdateRequest 是文档配置更新请求。
// 更新时不会直接重跑分块，除非后续调用 startChunk 或调度刷新。
type knowledgeDocumentUpdateRequest struct {
	DocName         string `json:"docName"`
	ProcessMode     string `json:"processMode"`
	ChunkStrategy   string `json:"chunkStrategy"`
	ChunkConfig     string `json:"chunkConfig"`
	PipelineID      string `json:"pipelineId"`
	SourceLocation  string `json:"sourceLocation"`
	ScheduleEnabled *int   `json:"scheduleEnabled"`
	ScheduleCron    string `json:"scheduleCron"`
}

// NewKnowledgeDocHandler 装配知识库文档处理器。
// 这里同时创建 pipeline/task 服务、调度服务和 token 估算器，因为文档入库需要跨越
// 文件存储、runtime 执行、向量写入、定时刷新和 chunk 统计。
func NewKnowledgeDocHandler(
	db *gorm.DB,
	fileStore storage.FileStore,
	vectorSvc vector.VectorStoreService,
	embSvc embedding.EmbeddingService,
	tika *internalparser.TikaParser,
	runtime *ingestion.Runtime,
) *KnowledgeDocHandler {
	pipelineSvc := ingestion.NewPipelineService(db)
	taskSvc := ingestion.NewTaskService(db, pipelineSvc, runtime)
	return &KnowledgeDocHandler{
		db:        db,
		fileStore: fileStore,
		tasks:     taskSvc,
		vectorSvc: vectorSvc,
		embSvc:    embSvc,
		tika:      tika,
		schedules: scheduler.NewKnowledgeDocumentScheduleService(db, 60),
		counter:   token.NewHeuristicCounter(),
	}
}

// RegisterRoutes 注册文档上传、分块、查询、更新、删除和 chunk 日志相关接口。
func (h *KnowledgeDocHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/knowledge-base/:kbId/docs/upload", h.upload)
	rg.POST("/knowledge-base/docs/:docId/chunk", h.startChunk)
	rg.DELETE("/knowledge-base/docs/:docId", h.delete)
	// Literal paths must be registered before /knowledge-base/docs/:docId (Gin/httprouter).
	rg.GET("/knowledge-base/docs/search", h.search)
	rg.GET("/knowledge-base/docs/:docId", h.get)
	rg.PUT("/knowledge-base/docs/:docId", h.update)
	rg.GET("/knowledge-base/:kbId/docs", h.page)
	rg.PATCH("/knowledge-base/docs/:docId/enable", h.enable)
	rg.GET("/knowledge-base/docs/:docId/chunk-logs", h.chunkLogs)
}

// upload 创建知识库文档记录。
// 对 file 来源，会先把 multipart 文件保存到 FileStore；对 url 来源，只记录地址。
// 此接口只把文档置为 pending，不直接执行分块，分块由 startChunk/worker 或调度触发。
func (h *KnowledgeDocHandler) upload(c *gin.Context) {
	kbID := c.Param("kbId")
	var req knowledgeDocumentUploadRequest
	if err := c.ShouldBind(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	file, _ := c.FormFile("file")
	normalizedReq, err := h.normalizeUploadRequest(c.Request.Context(), req, file)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, err.Error())
		return
	}
	// normalize 后，后续逻辑只处理“规范态”请求：
	// - sourceType 一定是 file/url
	// - file 模式必须有 multipart 文件
	// - url 模式会校验 sourceLocation、schedule 配置

	var storedPath string
	var docName string
	var fileType string
	var fileSize int64
	switch normalizedReq.SourceType {
	case "file":
		// 文件上传需要保存实际二进制，后续分块时再从 FileStore 读取。
		docName = file.Filename
		fileType = file.Header.Get("Content-Type")
		fileSize = file.Size
		if h.fileStore != nil {
			// 这里仅做“原始文件落盘/落对象存储”，不做解析与分块。
			// 设计上把 IO 与计算解耦：上传接口快返回，计算交给异步 worker。
			src, err := file.Open()
			if err != nil {
				response.FailWithCode(c, errcode.ClientError, "文件读取失败")
				return
			}
			defer src.Close()
			storedPath, err = h.fileStore.Save(file.Filename, src)
			if err != nil {
				slog.Error("file save failed", "err", err)
				response.FailWithCode(c, errcode.ServiceError, "文件保存失败")
				return
			}
		}
	case "url":
		// URL 文档不复制内容，只保存地址；分块或调度刷新时再拉取远端内容。
		docName = deriveDocNameFromLocation(normalizedReq.SourceLocation)
		storedPath = normalizedReq.SourceLocation
	}

	id := idgen.NextIDStr()
	// 文档主表保存处理模式、分块策略、调度配置和当前状态，是离线闭环的业务主记录。
	doc := entity.KnowledgeDocumentDO{
		BaseModel:      entity.BaseModel{ID: id},
		KBID:           kbID,
		DocName:        docName,
		SourceType:     normalizedReq.SourceType,
		FileType:       fileType,
		FileSize:       fileSize,
		SourceLocation: storedPath,
		ScheduleEnabled: func() int {
			if normalizedReq.ScheduleEnabled {
				return 1
			}
			return 0
		}(),
		ScheduleCron:  normalizedReq.ScheduleCron,
		ProcessMode:   normalizedReq.ProcessMode,
		ChunkStrategy: normalizedReq.ChunkStrategy,
		ChunkConfig:   normalizedReq.ChunkConfig,
		PipelineID:    normalizedReq.PipelineID,
		Status:        "pending",
		CreatedBy:     auth.GetUserID(c.Request.Context()),
	}
	if normalizedReq.SourceType == "file" {
		// fileURL 对文件来源指向存储路径，url 来源则保留原始访问地址。
		doc.FileURL = storedPath
	} else {
		doc.FileURL = normalizedReq.SourceLocation
	}
	if err := h.db.Create(&doc).Error; err != nil {
		slog.Error("knowledge document create failed", "kbId", kbID, "docId", id, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "文档创建失败")
		return
	}
	// 文档计数是运营侧指标，和分块状态无关，上传成功即 +1。
	h.db.Model(&entity.KnowledgeBaseDO{}).Where("id = ?", kbID).
		UpdateColumn("document_count", gorm.Expr("document_count + 1"))
	response.Success(c, doc)
}

// startChunk 把文档放入分块任务队列。
// 它先做并发状态保护，再同步调度配置，最后创建一条 pending 的 ingestion task，
// 由后台 worker 消费，避免 HTTP 请求长时间阻塞。
func (h *KnowledgeDocHandler) startChunk(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	// Load document
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.FailWithCode(c, errcode.ClientError, "文档不存在")
			return
		}
		slog.Error("startChunk load document failed", "docId", docID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档失败")
		return
	}

	status := strings.ToLower(strings.TrimSpace(doc.Status))
	if status == "running" || status == "pending" {
		// 防止用户重复点击或调度并发导致同一文档同时分块。
		response.FailWithCode(c, errcode.ClientError, "文档分块操作正在进行中，请稍后再试")
		return
	}

	// 先把文档切到 pending，再建任务：
	// - 成功路径：worker 会把 pending -> running -> success/failed
	// - 失败路径：enqueue 失败会显式置 failed，避免永远 pending
	if err := h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Update("status", "pending").Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "更新文档状态失败")
		return
	}
	doc.Status = "pending"
	if h.schedules != nil {
		// 手动触发时也同步一次 schedule，保证 cron 和文档配置保持一致。
		if err := h.schedules.UpsertSchedule(doc); err != nil {
			response.FailWithCode(c, errcode.ClientError, err.Error())
			return
		}
	}

	if err := h.enqueueChunkTask(c.Request.Context(), doc); err != nil {
		// 状态补偿：任务没建成时不能留 pending，否则会误导前端“仍在处理中”。
		_ = h.setDocStatusWithError(doc.ID, "failed", "创建分块任务失败: "+err.Error())
		response.FailWithCode(c, errcode.ServiceError, "创建分块任务失败")
		return
	}

	response.SuccessEmpty(c)
}

// runChunkPipeline executes the full document processing pipeline:
// load file -> parse -> chunk + embed -> index to vector store -> save chunks to DB.
func (h *KnowledgeDocHandler) runChunkPipeline(doc entity.KnowledgeDocumentDO) {
	// 当前作为兼容包装保留，实际错误处理由 processChunkPipeline 内部记录。
	_ = h.processChunkPipeline(doc)
}

const (
	chunkTaskSourceType = "knowledge_doc_chunk"
	chunkTaskStatusDone = "completed"
)

// StartChunkTaskWorker 启动本进程内的文档分块轮询 worker。
// worker 从 ingestion task 表中 claim pending 任务，再调用 processChunkPipeline。
func (h *KnowledgeDocHandler) StartChunkTaskWorker() {
	if h.db == nil {
		return
	}
	if h.queueStop != nil {
		// 已启动则直接返回，避免重复启动多个本地 worker。
		// 注意：多实例部署时仍会有“每实例一个 worker”，真正并发互斥依赖 claimNextChunkTask 的条件更新。
		return
	}
	h.queueStop = make(chan struct{})
	h.queueWG.Add(1)
	go h.chunkTaskWorkerLoop()
}

// StopChunkTaskWorker 停止分块 worker，并等待 goroutine 退出。
func (h *KnowledgeDocHandler) StopChunkTaskWorker() {
	if h.queueStop == nil {
		return
	}
	close(h.queueStop)
	h.queueWG.Wait()
	h.queueStop = nil
}

// chunkTaskWorkerLoop 每 2 秒尝试消费一个分块任务。
// 这里采用轻量轮询，任务 claim 逻辑在数据库事务中保证并发安全。
func (h *KnowledgeDocHandler) chunkTaskWorkerLoop() {
	defer h.queueWG.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.consumeOneChunkTask(context.Background())
		case <-h.queueStop:
			return
		}
	}
}

// consumeOneChunkTask 执行单个队列任务：claim -> 找文档 -> 标记 running ->
// 处理 pipeline -> 更新任务完成/失败状态。
func (h *KnowledgeDocHandler) consumeOneChunkTask(ctx context.Context) {
	task, ok, err := h.claimNextChunkTask(ctx)
	if err != nil || !ok {
		return
	}
	docID := strings.TrimSpace(parseChunkTaskDocID(task.MetadataJson))
	if docID == "" {
		// 没有 docId 的任务无法处理，标记完成避免 worker 永远卡在坏数据上。
		// 这里用 completed 而非 failed 是“隔离坏数据噪音”的权衡：不会反复重试同一条脏任务。
		_ = h.finishChunkTask(task.ID, chunkTaskStatusDone, "missing doc id")
		return
	}

	var doc entity.KnowledgeDocumentDO
	if err := h.db.WithContext(ctx).Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		_ = h.finishChunkTask(task.ID, "failed", "文档不存在: "+err.Error())
		return
	}
	if err := h.db.WithContext(ctx).Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Update("status", "running").Error; err != nil {
		_ = h.finishChunkTask(task.ID, "failed", "更新文档状态失败: "+err.Error())
		return
	}
	// 从这里开始文档处于 running，任何手工 chunk 变更接口都应拦截（见 knowledge_chunk_handler）。
	doc.Status = "running"

	execErr := h.processChunkPipeline(doc)
	if execErr != nil {
		// processChunkPipeline 内部已经写文档状态和错误日志，这里补任务状态。
		_ = h.finishChunkTask(task.ID, "failed", execErr.Error())
		return
	}
	_ = h.finishChunkTask(task.ID, chunkTaskStatusDone, "")
}

// claimNextChunkTask 在事务内抢占最早的 pending 分块任务。
// 先读候选任务，再用 status=pending 条件更新为 running；RowsAffected 为 0 说明
// 其他 worker 已抢先拿走该任务。
func (h *KnowledgeDocHandler) claimNextChunkTask(ctx context.Context) (entity.IngestionTaskDO, bool, error) {
	var claimed entity.IngestionTaskDO
	err := h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidate entity.IngestionTaskDO
		if err := tx.Where("source_type = ? AND status = ? AND deleted = 0", chunkTaskSourceType, "pending").
			Order("create_time ASC").
			First(&candidate).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil
			}
			return err
		}
		now := time.Now()
		// 核心并发点：WHERE status=pending 的条件更新是抢占锁。
		// 即使多个 worker 同时读到 candidate，也只有一个能把 pending 改成 running。
		res := tx.Model(&entity.IngestionTaskDO{}).
			Where("id = ? AND status = ? AND deleted = 0", candidate.ID, "pending").
			Updates(map[string]interface{}{
				"status":     "running",
				"started_at": &now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		candidate.Status = "running"
		candidate.StartedAt = &now
		claimed = candidate
		return nil
	})
	if err != nil {
		return entity.IngestionTaskDO{}, false, err
	}
	if claimed.ID == "" {
		return entity.IngestionTaskDO{}, false, nil
	}
	return claimed, true, nil
}

// enqueueChunkTask 创建文档分块任务，docId 放在 MetadataJson 中供 worker 解析。
func (h *KnowledgeDocHandler) enqueueChunkTask(ctx context.Context, doc entity.KnowledgeDocumentDO) error {
	payload, _ := json.Marshal(map[string]string{"docId": doc.ID})
	now := time.Now()
	task := entity.IngestionTaskDO{
		BaseModel:      entity.BaseModel{ID: idgen.NextIDStr()},
		SourceType:     chunkTaskSourceType,
		SourceLocation: doc.SourceLocation,
		SourceFileName: doc.DocName,
		Status:         "pending",
		MetadataJson:   string(payload),
		CreatedBy:      auth.GetUserID(ctx),
		UpdatedBy:      auth.GetUserID(ctx),
		StartedAt:      &now,
	}
	// 注意：这里 StartedAt 代表“进入队列时间”，真正执行开始会在 claim 时覆盖一次。
	return h.db.WithContext(ctx).Create(&task).Error
}

// finishChunkTask 更新队列任务最终状态和完成时间。
func (h *KnowledgeDocHandler) finishChunkTask(taskID, status, message string) error {
	now := time.Now()
	return h.db.Model(&entity.IngestionTaskDO{}).Where("id = ? AND deleted = 0", taskID).Updates(map[string]interface{}{
		"status":        status,
		"error_message": message,
		"completed_at":  &now,
	}).Error
}

// parseChunkTaskDocID 从任务 MetadataJson 中取出要处理的文档 ID。
func parseChunkTaskDocID(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload["docId"])
}

// setDocStatusWithError 设置文档状态，并在有错误信息时标记为 system 更新。
func (h *KnowledgeDocHandler) setDocStatusWithError(docID, status, message string) error {
	updates := map[string]interface{}{
		"status": status,
	}
	if message != "" {
		updates["updated_by"] = "system"
	}
	return h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Updates(updates).Error
}

// RefreshDocument 是调度器调用的刷新入口。
// 它绕过 HTTP 队列，直接把文档标记为 running 并同步执行完整分块链路。
func (h *KnowledgeDocHandler) RefreshDocument(ctx context.Context, docID string) error {
	var doc entity.KnowledgeDocumentDO
	if err := h.db.WithContext(ctx).Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		return err
	}
	if strings.EqualFold(doc.Status, "running") {
		return fmt.Errorf("document is already running")
	}
	// 调度入口走“同步直跑”而非 enqueue，目的是：
	// - 便于 schedule processor 在一个调用链内拿到成功/失败
	// - 减少调度器与本地队列的耦合复杂度
	if err := h.db.WithContext(ctx).Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Update("status", "running").Error; err != nil {
		return err
	}
	doc.Status = "running"
	return h.processChunkPipeline(doc)
}

// processChunkPipeline 执行文档从“源内容”到“DB chunk + 向量库”的完整闭环。
// 两种模式：
// 1. pipeline：使用用户配置的 ingestion pipeline，但跳过 indexer 内部写入；
// 2. chunk：走内置 parser + chunker 快速链路。
// 无论哪种模式，最终都由本函数统一删除旧 chunk、写新 chunk、写向量和更新文档状态。
func (h *KnowledgeDocHandler) processChunkPipeline(doc entity.KnowledgeDocumentDO) error {
	ctx := context.Background()
	start := time.Now()

	var kb entity.KnowledgeBaseDO
	if err := h.db.Where("id = ? AND deleted = 0", doc.KBID).First(&kb).Error; err != nil {
		// 文档必须属于有效知识库，否则无法确定 collection 和 embedding 模型。
		h.logChunkError(doc.ID, "知识库不存在: "+err.Error())
		h.setDocStatus(doc.ID, "failed")
		return err
	}

	rawBytes, err := h.loadDocumentBytes(doc)
	if err != nil {
		// 文件读取失败通常来自 FileStore 缺失、对象不存在或远端 URL 不可达。
		h.logChunkError(doc.ID, "读取文件内容失败: "+err.Error())
		h.setDocStatus(doc.ID, "failed")
		return err
	}

	mode := strings.ToLower(strings.TrimSpace(doc.ProcessMode))
	if mode == "" {
		mode = "chunk"
	}

	var ingestCtx *ingestion.IngestionContext
	switch mode {
	case "pipeline":
		if doc.PipelineID == "" {
			err = fmt.Errorf("Pipeline模式下Pipeline ID为空")
			h.logChunkError(doc.ID, err.Error())
			h.setDocStatus(doc.ID, "failed")
			return err
		}
		// pipeline 模式复用 ingestion.Runtime 的完整节点编排；SkipIndexerWrite 让
		// indexer 只准备 chunks，不直接写向量，避免和下面的统一持久化重复写。
		ingestCtx, err = h.tasks.RunRuntime(ctx, doc.PipelineID, &ingestion.IngestionContext{
			DocID:    doc.ID,
			KBID:     doc.KBID,
			TaskID:   doc.ID,
			FileName: doc.DocName,
			MimeType: doc.FileType,
			RawBytes: rawBytes,
			VectorSpaceID: &ingestion.VectorSpaceID{
				LogicalName: kb.CollectionName,
			},
			SkipIndexerWrite: true,
		})
	default:
		// chunk 模式是简化链路，只执行 parser 和 chunker，适合不需要自定义 pipeline 的文档。
		ingestCtx, err = h.runChunkMode(ctx, doc, kb, rawBytes)
	}
	if err != nil {
		slog.Error("chunk pipeline failed", "docID", doc.ID, "err", err)
		h.logChunkError(doc.ID, "分块流水线失败: "+err.Error())
		h.setDocStatus(doc.ID, "failed")
		return err
	}

	// 新一轮入库采用全量替换策略：先删旧 chunk，再写入本次结果。
	// 这是“覆盖式一致性”而非“增量合并”：
	// - 优点：不会残留旧 chunk / 旧索引
	// - 代价：中途失败时文档会短暂无 chunk（因此失败路径必须明确置 failed）
	if err := h.db.Where("doc_id = ?", doc.ID).Delete(&entity.KnowledgeChunkDO{}).Error; err != nil {
		h.logChunkError(doc.ID, "旧分块清理失败: "+err.Error())
		h.setDocStatus(doc.ID, "failed")
		return err
	}

	for i, chunk := range ingestCtx.Chunks {
		// DB chunk 行用于管理端展示和传统查询，向量库用于语义检索，两者共享 chunkID。
		chunkDO := h.buildChunkDO(doc, chunk.ID, chunk.Content, i)
		// 这里未显式检查 Create 错误是历史实现；按行为看，后续向量写失败会整体 failed。
		// 若要提升可靠性，下一步应把 DB 写入失败显式 return。
		h.db.Create(&chunkDO)
	}

	// chunk 行写入后再刷新向量，保证管理端可追溯每个向量对应的 DB chunk。
	if err := h.persistVectors(ctx, kb.CollectionName, doc.ID, ingestCtx.Chunks); err != nil {
		h.logChunkError(doc.ID, "向量写入失败: "+err.Error())
		h.setDocStatus(doc.ID, "failed")
		return err
	}

	h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ?", doc.ID).Updates(map[string]interface{}{
		"status":      "success",
		"chunk_count": len(ingestCtx.Chunks),
	})

	h.logChunkInfo(doc.ID, ingestCtx, mode, doc, time.Since(start).Milliseconds())
	slog.Info("chunk pipeline completed", "docID", doc.ID, "chunks", len(ingestCtx.Chunks))
	return nil
}

func (h *KnowledgeDocHandler) persistVectors(ctx context.Context, collectionName, docID string, chunks []ingestion.Chunk) error {
	if h.vectorSvc == nil {
		// 未配置向量服务时允许只保存 DB chunk，方便本地调试或降级运行。
		return nil
	}
	if collectionName == "" {
		return errcode.NewClientError("知识库未配置collectionName")
	}
	chunkData := make([]vector.ChunkData, 0, len(chunks))
	for i, chunk := range chunks {
		// 向量库内容做长度截断，避免超出底层向量存储字段限制。
		chunkData = append(chunkData, vector.ChunkData{
			ID:      chunk.ID,
			DocID:   docID,
			Index:   i,
			Content: truncateChunkContent(chunk.Content),
			Vector:  chunk.Vector,
			Metadata: func() map[string]string {
				if chunk.Metadata != nil {
					return chunk.Metadata
				}
				return map[string]string{}
			}(),
		})
	}
	if len(chunkData) == 0 {
		return nil
	}
	if err := h.vectorSvc.DeleteByDocID(ctx, collectionName, docID); err != nil {
		// 删除旧向量失败记录告警但继续写入；后续 IndexChunks 失败才视为本次入库失败。
		slog.Warn("delete old document vectors failed", "docID", docID, "err", err)
	}
	if err := h.vectorSvc.IndexChunks(ctx, collectionName, chunkData); err != nil {
		return err
	}
	return nil
}

// runChunkMode 执行内置轻量链路：parser -> chunker。
// 它不走 PipelineService，因此适合普通文档上传的默认分块模式；日志手动写入 context，
// 以便 logChunkInfo 能统计各阶段耗时和结果。
func (h *KnowledgeDocHandler) runChunkMode(ctx context.Context, doc entity.KnowledgeDocumentDO, kb entity.KnowledgeBaseDO, rawBytes []byte) (*ingestion.IngestionContext, error) {
	parserNode := ingestion.NewParserNode(h.tika)
	ingestCtx := &ingestion.IngestionContext{
		DocID:    doc.ID,
		KBID:     doc.KBID,
		TaskID:   doc.ID,
		FileName: doc.DocName,
		MimeType: doc.FileType,
		RawBytes: rawBytes,
		Metadata: map[string]interface{}{},
		VectorSpaceID: &ingestion.VectorSpaceID{
			LogicalName: kb.CollectionName,
		},
	}

	parserResult := parserNode.Execute(ctx, ingestCtx, ingestion.NodeConfig{NodeID: "parser", NodeType: "parser"})
	// 内置模式没有 engine 包装，因此这里手动追加 parser 节点日志。
	ingestCtx.AppendLog(ingestion.NodeLog{
		NodeID:    "parser",
		NodeType:  "parser",
		NodeOrder: 1,
		Message:   parserResult.Message,
		Success:   parserResult.Success,
		Error:     errorString(parserResult.Error),
		Output:    parserResult.Output,
	})
	if !parserResult.Success {
		return nil, parserResult.Error
	}

	chunkerNode := ingestion.NewChunkerNode(h.embSvc, 512, 128)
	// chunk 配置来自文档行和知识库 embeddingModel，转换为 ChunkerNode 能识别的 settings。
	chunkerResult := chunkerNode.Execute(ctx, ingestCtx, ingestion.NodeConfig{
		NodeID:   "chunker",
		NodeType: "chunker",
		Settings: buildChunkModeSettings(doc, kb),
	})
	// 同样手动追加 chunker 日志，使简化链路和 pipeline 链路的日志结构一致。
	ingestCtx.AppendLog(ingestion.NodeLog{
		NodeID:    "chunker",
		NodeType:  "chunker",
		NodeOrder: 2,
		Message:   chunkerResult.Message,
		Success:   chunkerResult.Success,
		Error:     errorString(chunkerResult.Error),
		Output:    chunkerResult.Output,
	})
	if !chunkerResult.Success {
		return nil, chunkerResult.Error
	}

	ingestCtx.Status = ingestion.IngestionStatusCompleted
	return ingestCtx, nil
}

// loadDocumentBytes 根据文档来源读取原始内容。
// URL 来源直接 HTTP GET；文件来源从 FileStore 读取上传时保存的路径。
func (h *KnowledgeDocHandler) loadDocumentBytes(doc entity.KnowledgeDocumentDO) ([]byte, error) {
	if strings.EqualFold(strings.TrimSpace(doc.SourceType), "url") {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, doc.SourceLocation, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("remote source returned status %d", resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	if h.fileStore == nil || strings.TrimSpace(doc.SourceLocation) == "" {
		return nil, fmt.Errorf("document source is unavailable")
	}
	reader, err := h.fileStore.Load(doc.SourceLocation)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// truncateChunkContent 限制写入向量库的文本长度，避免底层字段或 payload 过大。
func truncateChunkContent(content string) string {
	runes := []rune(content)
	if len(runes) <= 65535 {
		return content
	}
	return string(runes[:65535])
}

// setDocStatus 是文档状态的轻量更新 helper，主要用于错误分支。
func (h *KnowledgeDocHandler) setDocStatus(docID, status string) {
	h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ?", docID).Update("status", status)
}

// logChunkError 写入文档分块错误日志，供管理端 chunk-logs 接口查询。
func (h *KnowledgeDocHandler) logChunkError(docID, message string) {
	log := entity.KnowledgeDocumentChunkLogDO{
		BaseModel:    entity.BaseModel{ID: idgen.NextIDStr()},
		DocID:        docID,
		Status:       "ERROR",
		ErrorMessage: message,
	}
	h.db.Create(&log)
}

// logChunkInfo 汇总本次分块成功日志。
// 它按节点类型粗略拆分提取、分块/embedding、持久化耗时，便于排查慢阶段。
func (h *KnowledgeDocHandler) logChunkInfo(docID string, ctx *ingestion.IngestionContext, mode string, doc entity.KnowledgeDocumentDO, totalDuration int64) {
	var extractDuration int64
	var chunkDuration int64
	var embedDuration int64
	var persistDuration int64
	for _, log := range ctx.Logs {
		switch log.NodeType {
		case "fetcher", "parser":
			extractDuration += log.DurationMs
		case "chunker":
			chunkDuration += log.DurationMs
			embedDuration += log.DurationMs
		case "indexer":
			persistDuration += log.DurationMs
		}
	}
	log := entity.KnowledgeDocumentChunkLogDO{
		BaseModel:       entity.BaseModel{ID: idgen.NextIDStr()},
		DocID:           docID,
		Status:          "SUCCESS",
		ProcessMode:     mode,
		ChunkStrategy:   doc.ChunkStrategy,
		PipelineID:      doc.PipelineID,
		ExtractDuration: extractDuration,
		ChunkDuration:   chunkDuration,
		EmbedDuration:   embedDuration,
		PersistDuration: persistDuration,
		TotalDuration:   totalDuration,
		ChunkCount:      len(ctx.Chunks),
	}
	h.db.Create(&log)
}

func (h *KnowledgeDocHandler) delete(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.FailWithCode(c, errcode.ClientError, "文档不存在")
			return
		}
		slog.Error("delete load document failed", "docId", docID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档失败")
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块中，无法删除")
		return
	}
	var kb entity.KnowledgeBaseDO
	_ = h.db.Where("id = ? AND deleted = 0", doc.KBID).First(&kb).Error
	tx := h.db.Begin()
	tx.Model(&entity.KnowledgeDocumentDO{}).Where("id = ?", docID).Update("deleted", 1)
	tx.Where("doc_id = ?", docID).Delete(&entity.KnowledgeChunkDO{})
	tx.Where("doc_id = ?", docID).Delete(&entity.KnowledgeDocumentChunkLogDO{})
	tx.Model(&entity.KnowledgeBaseDO{}).Where("id = ?", doc.KBID).
		UpdateColumn("document_count", gorm.Expr("GREATEST(document_count - 1, 0)"))
	if err := tx.Commit().Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "删除文档失败")
		return
	}
	if h.schedules != nil {
		if err := h.schedules.DeleteByDocID(docID); err != nil {
			response.FailWithCode(c, errcode.ServiceError, "删除文档调度失败")
			return
		}
	}
	if h.vectorSvc != nil && strings.TrimSpace(kb.CollectionName) != "" {
		if err := h.vectorSvc.DeleteByDocID(c.Request.Context(), kb.CollectionName, docID); err != nil {
			slog.Warn("delete document vectors failed", "docID", docID, "err", err)
		}
	}
	response.SuccessEmpty(c)
}

func (h *KnowledgeDocHandler) get(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.FailWithCode(c, errcode.ClientError, "文档不存在")
			return
		}
		slog.Error("get knowledge document failed", "docId", docID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档失败")
		return
	}
	response.Success(c, doc)
}

func (h *KnowledgeDocHandler) update(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	var req knowledgeDocumentUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.FailWithCode(c, errcode.ClientError, "文档不存在")
			return
		}
		slog.Error("update load document failed", "docId", docID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档失败")
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块中，无法修改")
		return
	}
	updates, err := h.normalizeUpdateRequest(c.Request.Context(), doc, req)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, err.Error())
		return
	}
	if err := h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Updates(updates).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "更新文档失败")
		return
	}
	for key, value := range updates {
		switch key {
		case "doc_name":
			doc.DocName = fmt.Sprintf("%v", value)
		case "process_mode":
			doc.ProcessMode = fmt.Sprintf("%v", value)
		case "chunk_strategy":
			doc.ChunkStrategy = fmt.Sprintf("%v", value)
		case "chunk_config":
			doc.ChunkConfig = fmt.Sprintf("%v", value)
		case "pipeline_id":
			doc.PipelineID = fmt.Sprintf("%v", value)
		case "source_location":
			doc.SourceLocation = fmt.Sprintf("%v", value)
		case "schedule_cron":
			doc.ScheduleCron = fmt.Sprintf("%v", value)
		case "schedule_enabled":
			if enabled, ok := value.(int); ok {
				doc.ScheduleEnabled = enabled
			}
		}
	}
	if h.schedules != nil {
		if err := h.schedules.UpsertSchedule(doc); err != nil {
			response.FailWithCode(c, errcode.ClientError, err.Error())
			return
		}
	}
	response.SuccessEmpty(c)
}

func (h *KnowledgeDocHandler) page(c *gin.Context) {
	kbID := c.Param("kbId")
	pageNo, pageSize := response.ParsePage(c)
	keyword := c.Query("keyword")
	status := c.Query("status")

	var list []entity.KnowledgeDocumentDO
	var total int64
	q := h.db.Model(&entity.KnowledgeDocumentDO{}).Where("kb_id = ? AND deleted = 0", kbID)
	if keyword != "" {
		q = q.Where("doc_name LIKE ?", "%"+keyword+"%")
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	q.Count(&total)
	if err := q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("create_time DESC").Find(&list).Error; err != nil {
		slog.Error("knowledge document page find failed", "kbId", kbID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档列表失败")
		return
	}
	response.SuccessPage(c, list, total, pageNo, pageSize)
}

func (h *KnowledgeDocHandler) search(c *gin.Context) {
	keyword := c.Query("keyword")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "8"))
	var docs []entity.KnowledgeDocumentDO
	q := h.db.Model(&entity.KnowledgeDocumentDO{}).Where("deleted = 0")
	if keyword != "" {
		q = q.Where("doc_name LIKE ?", "%"+keyword+"%")
	}
	q.Limit(limit).Find(&docs)

	// Batch-fetch KB names so the frontend doesn't need a second request.
	kbIDs := make([]string, 0, len(docs))
	seen := make(map[string]bool)
	for _, d := range docs {
		if !seen[d.KBID] {
			kbIDs = append(kbIDs, d.KBID)
			seen[d.KBID] = true
		}
	}
	kbNameMap := make(map[string]string)
	if len(kbIDs) > 0 {
		var kbs []entity.KnowledgeBaseDO
		h.db.Model(&entity.KnowledgeBaseDO{}).
			Select("id, name").
			Where("id IN ? AND deleted = 0", kbIDs).
			Find(&kbs)
		for _, kb := range kbs {
			kbNameMap[kb.ID] = kb.Name
		}
	}

	type SearchVO struct {
		ID      string `json:"id"`
		DocName string `json:"docName"`
		KBID    string `json:"kbId"`
		KbName  string `json:"kbName"`
	}
	vos := make([]SearchVO, 0, len(docs))
	for _, d := range docs {
		vos = append(vos, SearchVO{
			ID:      d.ID,
			DocName: d.DocName,
			KBID:    d.KBID,
			KbName:  kbNameMap[d.KBID],
		})
	}
	response.Success(c, vos)
}

func (h *KnowledgeDocHandler) enable(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	value := c.Query("value") == "true"
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.FailWithCode(c, errcode.ClientError, "文档不存在")
			return
		}
		slog.Error("enable load document failed", "docId", docID, "err", err)
		response.FailWithCode(c, errcode.ServiceError, "查询文档失败")
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块中，无法修改")
		return
	}
	if value && doc.ChunkCount == 0 {
		response.SuccessEmpty(c)
		return
	}
	var kb entity.KnowledgeBaseDO
	_ = h.db.Where("id = ? AND deleted = 0", doc.KBID).First(&kb).Error
	tx := h.db.Begin()
	if value {
		if err := h.reindexDocumentChunks(c.Request.Context(), kb, doc); err != nil {
			tx.Rollback()
			response.FailWithCode(c, errcode.ServiceError, "启用文档失败")
			return
		}
		tx.Model(&entity.KnowledgeChunkDO{}).Where("doc_id = ? AND deleted = 0", docID).Updates(map[string]interface{}{
			"enabled":    1,
			"updated_by": auth.GetUserID(c.Request.Context()),
		})
	} else {
		if h.vectorSvc != nil && strings.TrimSpace(kb.CollectionName) != "" {
			if err := h.vectorSvc.DeleteByDocID(c.Request.Context(), kb.CollectionName, docID); err != nil {
				tx.Rollback()
				response.FailWithCode(c, errcode.ServiceError, "禁用文档失败")
				return
			}
		}
		tx.Model(&entity.KnowledgeChunkDO{}).Where("doc_id = ? AND deleted = 0", docID).Updates(map[string]interface{}{
			"enabled":    0,
			"updated_by": auth.GetUserID(c.Request.Context()),
		})
	}
	tx.Model(&entity.KnowledgeDocumentDO{}).Where("id = ? AND deleted = 0", docID).Updates(map[string]interface{}{
		"enabled":    boolToInt(value),
		"updated_by": auth.GetUserID(c.Request.Context()),
	})
	if err := tx.Commit().Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "更新文档状态失败")
		return
	}
	doc.Enabled = entity.FlexEnabled(boolToInt(value))
	if h.schedules != nil {
		if err := h.schedules.SyncScheduleIfExists(doc); err != nil {
			response.FailWithCode(c, errcode.ClientError, err.Error())
			return
		}
	}
	response.SuccessEmpty(c)
}

func (h *KnowledgeDocHandler) chunkLogs(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	pageNo, pageSize := response.ParsePage(c)

	var logs []entity.KnowledgeDocumentChunkLogDO
	var total int64
	q := h.db.Model(&entity.KnowledgeDocumentChunkLogDO{}).Where("doc_id = ? AND deleted = 0", docID)
	q.Count(&total)
	q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("create_time DESC").Find(&logs)
	response.SuccessPage(c, logs, total, pageNo, pageSize)
}

// buildChunkModeSettings 将文档分块配置和知识库 embedding 模型合并成 ChunkerNode settings。
// 历史字段 chunkOverlap 会映射到 overlapSize，兼容前端/旧配置的命名。
func buildChunkModeSettings(doc entity.KnowledgeDocumentDO, kb entity.KnowledgeBaseDO) json.RawMessage {
	settings := map[string]interface{}{
		"strategy":       strings.TrimSpace(doc.ChunkStrategy),
		"embeddingModel": strings.TrimSpace(kb.EmbeddingModel),
	}
	if settings["strategy"] == "" {
		settings["strategy"] = "fixed_size"
	}

	if strings.TrimSpace(doc.ChunkConfig) != "" {
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(doc.ChunkConfig), &cfg); err == nil {
			for key, value := range cfg {
				settings[key] = value
			}
			if overlap, ok := cfg["chunkOverlap"]; ok {
				settings["overlapSize"] = overlap
			}
		}
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return nil
	}
	return data
}

// errorString 把 error 安全转成字符串，便于写入节点日志。
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// normalizedKnowledgeDocumentRequest 是上传请求校验后的内部表示。
type normalizedKnowledgeDocumentRequest struct {
	SourceType      string
	SourceLocation  string
	ScheduleEnabled bool
	ScheduleCron    string
	ProcessMode     string
	ChunkStrategy   string
	ChunkConfig     string
	PipelineID      string
}

// normalizeUploadRequest 校验上传请求并补齐处理配置。
// 它负责区分 file/url 必填项、校验调度 cron、校验 chunk/pipeline 模式参数。
func (h *KnowledgeDocHandler) normalizeUploadRequest(ctx context.Context, req knowledgeDocumentUploadRequest, file *multipart.FileHeader) (*normalizedKnowledgeDocumentRequest, error) {
	sourceType, err := normalizeSourceType(req.SourceType)
	if err != nil {
		return nil, err
	}
	processMode, err := normalizeProcessMode(req.ProcessMode)
	if err != nil {
		return nil, err
	}
	normalized := &normalizedKnowledgeDocumentRequest{
		SourceType:      sourceType,
		SourceLocation:  strings.TrimSpace(req.SourceLocation),
		ProcessMode:     processMode,
		ScheduleCron:    strings.TrimSpace(req.ScheduleCron),
		PipelineID:      strings.TrimSpace(req.PipelineID),
		ChunkConfig:     normalizeJSONString(req.ChunkConfig),
		ScheduleEnabled: req.ScheduleEnabled != nil && *req.ScheduleEnabled,
	}
	if sourceType == "file" {
		// file 来源必须有 multipart 文件；sourceLocation 由 FileStore 保存后生成。
		if file == nil {
			return nil, fmt.Errorf("上传文件不能为空")
		}
	} else if normalized.SourceLocation == "" {
		// url 来源不上传文件，必须提供远端地址。
		return nil, fmt.Errorf("来源地址不能为空")
	}
	if err := validateSchedule(sourceType, normalized.ScheduleEnabled, normalized.ScheduleCron); err != nil {
		return nil, err
	}
	chunkStrategy, chunkConfig, pipelineID, err := h.normalizeProcessingSettings(ctx, processMode, strings.TrimSpace(req.ChunkStrategy), normalized.ChunkConfig, normalized.PipelineID)
	if err != nil {
		return nil, err
	}
	normalized.ChunkStrategy = chunkStrategy
	normalized.ChunkConfig = chunkConfig
	normalized.PipelineID = pipelineID
	return normalized, nil
}

// normalizeUpdateRequest 校验文档更新请求并生成数据库 updates map。
// 它会沿用文档现有配置作为默认值，只覆盖请求中显式传入的字段。
func (h *KnowledgeDocHandler) normalizeUpdateRequest(ctx context.Context, doc entity.KnowledgeDocumentDO, req knowledgeDocumentUpdateRequest) (map[string]interface{}, error) {
	docName := strings.TrimSpace(req.DocName)
	if docName == "" {
		return nil, fmt.Errorf("文档名称不能为空")
	}
	updates := map[string]interface{}{
		"doc_name":   docName,
		"updated_by": auth.GetUserID(ctx),
	}

	processMode := strings.TrimSpace(doc.ProcessMode)
	if strings.TrimSpace(req.ProcessMode) != "" {
		var err error
		processMode, err = normalizeProcessMode(req.ProcessMode)
		if err != nil {
			return nil, err
		}
	}
	chunkStrategyInput := strings.TrimSpace(req.ChunkStrategy)
	if chunkStrategyInput == "" {
		chunkStrategyInput = strings.TrimSpace(doc.ChunkStrategy)
	}
	chunkConfigInput := strings.TrimSpace(doc.ChunkConfig)
	if normalizeJSONString(req.ChunkConfig) != "" {
		chunkConfigInput = normalizeJSONString(req.ChunkConfig)
	}
	pipelineIDInput := strings.TrimSpace(doc.PipelineID)
	if strings.TrimSpace(req.PipelineID) != "" {
		pipelineIDInput = strings.TrimSpace(req.PipelineID)
	}
	chunkStrategy, chunkConfig, pipelineID, err := h.normalizeProcessingSettings(ctx, processMode, chunkStrategyInput, chunkConfigInput, pipelineIDInput)
	if err != nil {
		return nil, err
	}
	updates["process_mode"] = processMode
	updates["chunk_strategy"] = chunkStrategy
	updates["chunk_config"] = chunkConfig
	updates["pipeline_id"] = pipelineID

	if strings.EqualFold(strings.TrimSpace(doc.SourceType), "url") {
		// 只有 URL 文档支持定时刷新；文件上传来源没有远端可拉取地址。
		sourceLocation := strings.TrimSpace(doc.SourceLocation)
		if strings.TrimSpace(req.SourceLocation) != "" {
			sourceLocation = strings.TrimSpace(req.SourceLocation)
			updates["source_location"] = sourceLocation
		}
		scheduleEnabled := doc.ScheduleEnabled
		if req.ScheduleEnabled != nil {
			scheduleEnabled = *req.ScheduleEnabled
			updates["schedule_enabled"] = scheduleEnabled
		}
		scheduleCron := strings.TrimSpace(doc.ScheduleCron)
		if strings.TrimSpace(req.ScheduleCron) != "" {
			scheduleCron = strings.TrimSpace(req.ScheduleCron)
			updates["schedule_cron"] = scheduleCron
		}
		if err := validateSchedule("url", scheduleEnabled == 1, scheduleCron); err != nil {
			return nil, err
		}
		if scheduleEnabled == 1 && strings.TrimSpace(sourceLocation) == "" {
			return nil, fmt.Errorf("启用定时拉取时必须设置来源地址")
		}
	}

	return updates, nil
}

// normalizeProcessingSettings 校验 chunk/pipeline 两种处理模式的互斥配置。
// chunk 模式要求分块策略与参数；pipeline 模式要求 pipelineID 存在且有效。
func (h *KnowledgeDocHandler) normalizeProcessingSettings(ctx context.Context, processMode, chunkStrategy, chunkConfig, pipelineID string) (string, string, string, error) {
	switch processMode {
	case "chunk":
		strategy, err := normalizeChunkStrategy(chunkStrategy)
		if err != nil {
			return "", "", "", err
		}
		cfg, err := validateChunkConfig(strategy, chunkConfig)
		if err != nil {
			return "", "", "", err
		}
		return strategy, cfg, "", nil
	case "pipeline":
		if strings.TrimSpace(pipelineID) == "" {
			return "", "", "", fmt.Errorf("使用Pipeline模式时，必须指定Pipeline ID")
		}
		var count int64
		if err := h.db.WithContext(ctx).Model(&entity.IngestionPipelineDO{}).Where("id = ? AND deleted = 0", pipelineID).Count(&count).Error; err != nil {
			return "", "", "", err
		}
		if count == 0 {
			return "", "", "", fmt.Errorf("指定的Pipeline不存在: %s", pipelineID)
		}
		return "", "", pipelineID, nil
	default:
		return "", "", "", fmt.Errorf("不支持的处理模式")
	}
}

// normalizeSourceType 标准化来源类型，目前文档管理只支持 file 和 url。
func normalizeSourceType(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "file", "localfile", "local_file":
		return "file", nil
	case "url":
		return "url", nil
	case "":
		return "", fmt.Errorf("来源类型不能为空")
	default:
		return "", fmt.Errorf("不支持的来源类型")
	}
}

// normalizeProcessMode 标准化处理模式：chunk 是内置链路，pipeline 是自定义流水线。
func normalizeProcessMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "chunk":
		return "chunk", nil
	case "pipeline":
		return "pipeline", nil
	case "":
		return "", fmt.Errorf("处理模式不能为空")
	default:
		return "", fmt.Errorf("不支持的处理模式")
	}
}

// normalizeChunkStrategy 标准化分块策略名称，兼容横线、下划线和空格差异。
func normalizeChunkStrategy(value string) (string, error) {
	switch strings.NewReplacer("-", "_", " ", "").Replace(strings.ToLower(strings.TrimSpace(value))) {
	case "fixed_size":
		return "fixed_size", nil
	case "structure_aware":
		return "structure_aware", nil
	case "":
		return "", fmt.Errorf("分块策略不能为空")
	default:
		return "", fmt.Errorf("Unknown chunk strategy: %s", value)
	}
}

// validateChunkConfig 校验分块配置 JSON，以及对应策略所需的关键字段。
func validateChunkConfig(strategy, raw string) (string, error) {
	raw = normalizeJSONString(raw)
	if raw == "" {
		return "", nil
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", fmt.Errorf("分块参数JSON格式不合法")
	}
	for _, key := range requiredChunkConfigKeys(strategy) {
		if _, ok := cfg[key]; !ok {
			return "", fmt.Errorf("分块参数缺少必要字段: %s", key)
		}
	}
	return raw, nil
}

// requiredChunkConfigKeys 返回不同分块策略所需的配置字段。
func requiredChunkConfigKeys(strategy string) []string {
	switch strategy {
	case "fixed_size":
		return []string{"chunkSize", "overlapSize"}
	default:
		return []string{"targetChars", "overlapChars", "maxChars", "minChars"}
	}
}

// validateSchedule 校验 URL 文档的定时刷新配置。
// 只有 URL 来源允许开启调度，并要求 cron 合法且执行间隔不少于 60 秒。
func validateSchedule(sourceType string, enabled bool, cronExpr string) error {
	if sourceType != "url" {
		return nil
	}
	if !enabled {
		return nil
	}
	if strings.TrimSpace(cronExpr) == "" {
		return fmt.Errorf("启用定时拉取时必须设置定时表达式")
	}
	parser := cron.NewParser(
		cron.SecondOptional |
			cron.Minute |
			cron.Hour |
			cron.Dom |
			cron.Month |
			cron.Dow |
			cron.Descriptor,
	)
	schedule, err := parser.Parse(strings.TrimSpace(cronExpr))
	if err != nil {
		return fmt.Errorf("定时表达式不合法")
	}
	now := time.Now()
	first := schedule.Next(now)
	second := schedule.Next(first)
	if first.IsZero() || second.IsZero() {
		return fmt.Errorf("定时表达式不合法")
	}
	if second.Sub(first) < time.Minute {
		return fmt.Errorf("定时执行间隔不能小于60秒")
	}
	return nil
}

// deriveDocNameFromLocation 从 URL/路径末段推导默认文档名。
func deriveDocNameFromLocation(location string) string {
	location = strings.TrimSpace(location)
	if location == "" {
		return ""
	}
	if idx := strings.LastIndex(location, "/"); idx >= 0 && idx < len(location)-1 {
		return location[idx+1:]
	}
	return location
}

// normalizeJSONString 统一清理 JSON 字符串字段，空白视为未配置。
func normalizeJSONString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// boolToInt 将布尔值转为数据库中使用的 0/1。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// buildChunkDO 将 runtime 产出的 chunk 转为数据库 chunk 行。
// 这里计算 content hash、字符数和估算 token 数，供去重、统计和管理端展示使用。
func (h *KnowledgeDocHandler) buildChunkDO(doc entity.KnowledgeDocumentDO, chunkID, content string, chunkIndex int) entity.KnowledgeChunkDO {
	return entity.KnowledgeChunkDO{
		BaseModel:   entity.BaseModel{ID: chunkID},
		DocID:       doc.ID,
		KBID:        doc.KBID,
		ChunkIndex:  chunkIndex,
		Content:     content,
		ContentHash: calculateChunkContentHash(content),
		CharCount:   len([]rune(content)),
		TokenCount:  h.countTokens(content),
		Enabled:     entity.FlexEnabled(1),
		CreatedBy:   doc.CreatedBy,
		UpdatedBy:   doc.CreatedBy,
	}
}

// reindexDocumentChunks 根据数据库中已有 chunk 重新生成向量。
// 用于手动编辑 chunk 内容或启用/禁用后需要同步向量库的场景。
func (h *KnowledgeDocHandler) reindexDocumentChunks(ctx context.Context, kb entity.KnowledgeBaseDO, doc entity.KnowledgeDocumentDO) error {
	if h.vectorSvc == nil || h.embSvc == nil || strings.TrimSpace(kb.CollectionName) == "" {
		return nil
	}
	var chunks []entity.KnowledgeChunkDO
	if err := h.db.Where("doc_id = ? AND deleted = 0", doc.ID).Order("chunk_index ASC").Find(&chunks).Error; err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	texts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		texts = append(texts, chunk.Content)
	}
	var vectors [][]float32
	var err error
	if selectable, ok := h.embSvc.(embedding.ModelSelectableEmbeddingService); ok && strings.TrimSpace(kb.EmbeddingModel) != "" {
		// 知识库指定 embedding 模型时优先使用指定模型重建向量。
		vectors, err = selectable.EmbedWithModelID(ctx, kb.EmbeddingModel, texts)
	} else {
		vectors, err = h.embSvc.Embed(ctx, texts)
	}
	if err != nil {
		return err
	}
	chunkData := make([]vector.ChunkData, 0, len(chunks))
	for i, chunk := range chunks {
		vectorValue := []float32(nil)
		if i < len(vectors) {
			vectorValue = vectors[i]
		}
		chunkData = append(chunkData, vector.ChunkData{
			ID:      chunk.ID,
			DocID:   doc.ID,
			Index:   chunk.ChunkIndex,
			Content: truncateChunkContent(chunk.Content),
			Vector:  vectorValue,
			Metadata: map[string]string{
				"kbId":  doc.KBID,
				"docId": doc.ID,
			},
		})
	}
	if err := h.vectorSvc.DeleteByDocID(ctx, kb.CollectionName, doc.ID); err != nil {
		return err
	}
	return h.vectorSvc.IndexChunks(ctx, kb.CollectionName, chunkData)
}

// countTokens 使用启发式计数器估算 chunk token 数。
func (h *KnowledgeDocHandler) countTokens(text string) int {
	if h.counter == nil {
		return 0
	}
	return h.counter.Count(text)
}

// calculateChunkContentHash 计算 chunk 内容 SHA-256，用于内容变更识别。
func calculateChunkContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
