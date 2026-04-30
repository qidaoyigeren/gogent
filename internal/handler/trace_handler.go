package handler

import (
	"gogent/internal/service"
	"gogent/pkg/errcode"
	"gogent/pkg/response"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TraceHandler 薄封装：db 本身不用但保留以便未来扩展；真正的查询走 querySvc。
type TraceHandler struct {
	db       *gorm.DB
	querySvc *service.RagTraceQueryService
}

func NewTraceHandler(db *gorm.DB) *TraceHandler {
	return &TraceHandler{db: db, querySvc: service.NewRagTraceQueryService(db)}
}

func (h *TraceHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/rag/traces/runs", h.pageRuns)
	rg.GET("/rag/traces/runs/:traceId", h.detail)
	rg.GET("/rag/traces/runs/:traceId/nodes", h.nodes)
}

// pageRuns 按 query 参数过滤 + 分页返回 run 列表；所有过滤字段都是可选的。
// response.SuccessPage 会拼 {list, total, pageNo, pageSize} 的统一分页结构。
func (h *TraceHandler) pageRuns(c *gin.Context) {
	pageNo, pageSize := response.ParsePage(c)
	traceID := c.Query("traceId")
	convID := c.Query("conversationId")
	taskID := c.Query("taskId")
	status := c.Query("status")

	vos, total, err := h.querySvc.PageRuns(service.TraceRunPageRequest{
		PageNo: pageNo, PageSize: pageSize,
		TraceID: traceID, ConversationID: convID, TaskID: taskID, Status: status,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.SuccessPage(c, vos, total, pageNo, pageSize)
}

// detail 返回 trace 全貌：顶部 run 卡片 + 中间 nodes 时间线。
// 找不到 run 时返回业务错误码（前端可跳回列表），而非 500。
func (h *TraceHandler) detail(c *gin.Context) {
	traceID := c.Param("traceId")
	detail, err := h.querySvc.GetDetail(traceID)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, "链路不存在")
		return
	}
	response.Success(c, gin.H{
		"run":   detail.Run,
		"nodes": detail.Nodes,
	})
}

// nodes 仅返回节点列表；详情页周期性轮询刷新进度时用，比 detail 更轻量。
func (h *TraceHandler) nodes(c *gin.Context) {
	traceID := c.Param("traceId")
	nodes, err := h.querySvc.ListNodes(traceID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Success(c, nodes)
}
