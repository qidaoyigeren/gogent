package handler

import (
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/internal/ingestion"
	"gogent/pkg/errcode"
	"gogent/pkg/response"
	"io"
	"net/http"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type IngestionHandler struct {
	db       *gorm.DB
	pipeline *ingestion.PipelineService
	task     *ingestion.TaskService
}

// NewIngestionHandler 创建 ingestion 管理端 Handler，负责 pipeline 定义和任务实例 API。
func NewIngestionHandler(db *gorm.DB, pipeline *ingestion.PipelineService, task *ingestion.TaskService) *IngestionHandler {
	return &IngestionHandler{db: db, pipeline: pipeline, task: task}
}

// RegisterRoutes 注册流水线 CRUD、任务创建/上传和任务日志查询接口。
func (h *IngestionHandler) RegisterRoutes(rg *gin.RouterGroup) {
	// Pipeline
	rg.POST("/ingestion/pipelines", h.createPipeline)
	rg.PUT("/ingestion/pipelines/:id", h.updatePipeline)
	rg.GET("/ingestion/pipelines/:id", h.getPipeline)
	rg.GET("/ingestion/pipelines", h.pagePipelines)
	rg.DELETE("/ingestion/pipelines/:id", h.deletePipeline)
	// Task
	rg.POST("/ingestion/tasks", h.createTask)
	rg.POST("/ingestion/tasks/upload", h.uploadTask)
	rg.GET("/ingestion/tasks/:id", h.getTask)
	rg.GET("/ingestion/tasks/:id/nodes", h.taskNodes)
	rg.GET("/ingestion/tasks", h.pageTasks)
}

// --- Pipeline ---

// createPipeline 接收前端提交的节点拓扑，并通过 PipelineService 持久化定义。
func (h *IngestionHandler) createPipeline(c *gin.Context) {
	var req struct {
		Name        string                 `json:"name" binding:"required"`
		Description string                 `json:"description"`
		Nodes       []ingestion.NodeConfig `json:"nodes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误: "+err.Error())
		return
	}
	userID := auth.GetUserID(c.Request.Context())
	view, err := h.pipeline.Create(c.Request.Context(), userID, ingestion.PipelineMutation{
		Name:        req.Name,
		Description: strPtr(req.Description),
		Nodes:       req.Nodes,
	})
	// 这里不做节点图合法性二次校验，交由 PipelineService/OrderPipelineNodes 统一约束。
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Success(c, view)
}

// updatePipeline 支持只更新主信息，也支持提交 nodes 时整体替换节点拓扑。
func (h *IngestionHandler) updatePipeline(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string                 `json:"name"`
		Description *string                `json:"description"`
		Nodes       []ingestion.NodeConfig `json:"nodes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	userID := auth.GetUserID(c.Request.Context())
	// Nodes 为 nil 表示不改节点；空切片表示显式清空节点。
	// 这是前端编辑器最关键契约：nil 与 [] 语义不同，避免“只改名称却误清空拓扑”。
	updateNodes := req.Nodes != nil
	view, err := h.pipeline.Update(c.Request.Context(), id, userID, ingestion.PipelineMutation{
		Name:        req.Name,
		Description: req.Description,
		Nodes:       req.Nodes,
	}, updateNodes)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Success(c, view)
}

// getPipeline 查询单条 pipeline 及排序后的节点列表。
func (h *IngestionHandler) getPipeline(c *gin.Context) {
	id := c.Param("id")
	view, err := h.pipeline.Get(c.Request.Context(), id)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, "流水线不存在")
		return
	}
	response.Success(c, view)
}

// pagePipelines 查询 pipeline 管理列表，支持 keyword 模糊搜索。
func (h *IngestionHandler) pagePipelines(c *gin.Context) {
	pageNo, pageSize := response.ParsePage(c)
	keyword := c.Query("keyword")
	views, total, err := h.pipeline.Page(c.Request.Context(), pageNo, pageSize, keyword)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.SuccessPage(c, views, total, pageNo, pageSize)
}

// deletePipeline 软删除 pipeline 定义及其节点。
func (h *IngestionHandler) deletePipeline(c *gin.Context) {
	id := c.Param("id")
	if err := h.pipeline.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.SuccessEmpty(c)
}

// --- Task ---

// createTask 使用来源地址创建并立即执行一个 ingestion task。
// 它适合 URL/本地路径类来源，文件上传场景走 uploadTask。
func (h *IngestionHandler) createTask(c *gin.Context) {
	var req struct {
		PipelineID     string `json:"pipelineId" binding:"required"`
		SourceType     string `json:"sourceType"`
		SourceLocation string `json:"sourceLocation"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	userID := auth.GetUserID(c.Request.Context())
	srcType, err := ingestion.ParseSourceType(req.SourceType)
	if err != nil {
		// 兼容旧调用未传 sourceType 的情况，默认按 file 处理。
		srcType = ingestion.SourceTypeFile
	}
	// createTask 偏“地址型任务”（url/path），大文件二进制走 uploadTask，避免 JSON 传大 payload。
	result, err := h.task.Execute(c.Request.Context(), ingestion.ExecuteTaskRequest{
		PipelineID: req.PipelineID,
		Source: ingestion.DocumentSource{
			Type:     srcType,
			Location: req.SourceLocation,
		},
		CreatedBy: userID,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Success(c, result)
}

// uploadTask 读取 multipart 文件字节，检测 MIME 后交给 TaskService 执行。
func (h *IngestionHandler) uploadTask(c *gin.Context) {
	pipelineID := c.PostForm("pipelineId")
	if pipelineID == "" {
		response.FailWithCode(c, errcode.ClientError, "请传入pipelineId")
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, "请上传文件")
		return
	}
	f, err := file.Open()
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		response.Fail(c, err)
		return
	}

	// Detect MIME type from bytes
	// 使用文件内容检测比信任上传 Header 更可靠，parser 节点会使用该 MIME 选择解析器。
	mime := mimetype.Detect(raw)
	mimeType := mime.String()
	userID := auth.GetUserID(c.Request.Context())

	result, err := h.task.Execute(c.Request.Context(), ingestion.ExecuteTaskRequest{
		PipelineID: pipelineID,
		Source: ingestion.DocumentSource{
			Type:     ingestion.SourceTypeFile,
			FileName: file.Filename,
		},
		RawBytes:  raw,
		MimeType:  mimeType,
		CreatedBy: userID,
	})
	// Execute 是同步执行：接口返回即代表任务已经完成或失败，便于调试 pipeline。
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Success(c, result)
}

// getTask 查询任务主表，包含状态、chunk 数、错误和汇总日志。
func (h *IngestionHandler) getTask(c *gin.Context) {
	id := c.Param("id")
	var t entity.IngestionTaskDO
	if err := h.db.Where("id = ? AND deleted = 0", id).First(&t).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "任务不存在")
		return
	}
	// 主表用于列表/详情首屏；节点级耗时和输出摘要由 taskNodes 补充。
	response.Success(c, t)
}

// taskNodes 查询任务的逐节点执行明细，按 node_order 展示执行顺序。
func (h *IngestionHandler) taskNodes(c *gin.Context) {
	id := c.Param("id")
	var nodes []entity.IngestionTaskNodeDO
	// 按 node_order 展示“逻辑执行顺序”，create_time 只是并列稳定排序兜底。
	h.db.Where("task_id = ? AND deleted = 0", id).Order("node_order ASC, create_time ASC").Find(&nodes)
	response.Success(c, nodes)
}

// pageTasks 分页查询任务实例，可按状态过滤。
func (h *IngestionHandler) pageTasks(c *gin.Context) {
	pageNo, pageSize := response.ParsePage(c)
	status := c.Query("status")
	var list []entity.IngestionTaskDO
	var total int64
	q := h.db.Model(&entity.IngestionTaskDO{}).Where("deleted = 0")
	if status != "" {
		// 状态值不在此处白名单校验，交由查询结果自然为空；便于兼容未来新增状态。
		q = q.Where("status = ?", status)
	}
	q.Count(&total)
	q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("create_time DESC").Find(&list)
	response.SuccessPage(c, list, total, pageNo, pageSize)
}

// strPtr converts a string to a *string.
func strPtr(s string) *string { return &s }

// Ensure net/http import is used.
var _ = http.StatusOK
