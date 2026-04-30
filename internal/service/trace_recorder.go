package service

import (
	"fmt"
	"gogent/internal/entity"
	"gogent/pkg/idgen"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// TraceRecorder 负责把一次 RAG 问答的 run + 多个 node 落库。
// 非线程安全：仅在单个请求的 doChat 内按顺序使用；并发节点（如 MCP 并行）
// 通过 exec 回调内部的本地变量聚合后再落一次 node。
type TraceRecorder struct {
	db      *gorm.DB  // 允许为 nil：测试或未接 DB 时走 no-op
	enabled bool      // 全局开关（见 RAGChatService.traceEnabled）
	traceID string    // StartRun 时生成；RecordNode 用来回指
	runID   string    // run 记录主键
	start   time.Time // run 开始时间，FinishRun 计算总耗时
	order   int       // 递增序号，作为 node 排序字段
}

// NewTraceRecorder 构造一个 recorder。即使 db 为 nil 也不会立刻报错，
// 所有写操作都会在各方法入口短路。
func NewTraceRecorder(db *gorm.DB, enabled bool) *TraceRecorder {
	return &TraceRecorder{db: db, enabled: enabled}
}

// StartRun 写入一条 RUNNING 的 run 记录，并初始化 traceID/runID/start。
// 参数：
//
//	conversationID - 会话 ID（跨多次问答）
//	taskID         - 流式 SSE 任务 ID（可空）
//	userID         - 发起用户（查询时支持带出 username）
//	question       - 原始问题（截断 1000 字符存 extra_data，避免 TEXT 爆炸）
//
// 失败只 warn；不抛错以免影响主流程。
func (t *TraceRecorder) StartRun(conversationID, taskID, userID, question string) {
	if !t.enabled || t.db == nil {
		return
	}
	t.traceID = idgen.NextIDStr()
	t.runID = idgen.NextIDStr()
	t.start = time.Now()
	t.order = 0

	run := entity.RagTraceRunDO{
		BaseModel:      entity.BaseModel{ID: t.runID},
		TraceID:        t.traceID,
		TraceName:      "RAG-Chat",                         // 固定分类名，前端可按 TraceName 过滤
		EntryMethod:    "orchestrator.RAGChatService#Chat", // 入口方法，方便跨服务定位
		ConversationID: conversationID,
		TaskID:         taskID,
		UserID:         userID,
		Status:         "RUNNING",
		StartTime:      &t.start,
		ExtraData:      truncate(question, 1000),
	}
	if err := t.db.Create(&run).Error; err != nil {
		slog.Warn("trace: failed to start run", "err", err)
	}
}

// RecordNode 记录某个阶段（node）：先插一条 RUNNING，再执行 exec，再按结果更新终态。
// 语义：
//   - enabled=false 或 db=nil → 直接返回 exec() 的结果（no-op）
//   - exec 返回 (output, err)，output 会被截断到 2000 存进 extra_data
//   - err != nil 时 node.status=ERROR 且 error_message 截断 1000
//   - 即使写库失败（忽略返回值），exec 的结果仍正常返回给上层编排
//
// 这样设计确保“观测失败不影响业务”。
func (t *TraceRecorder) RecordNode(nodeName, nodeType, input string, exec func() (string, error)) (string, error) {
	if !t.enabled || t.db == nil {
		return exec()
	}

	t.order++
	nodeID := idgen.NextIDStr()
	nodeStart := time.Now()

	node := entity.RagTraceNodeDO{
		BaseModel: entity.BaseModel{ID: idgen.NextIDStr()},
		TraceID:   t.traceID,
		NodeID:    nodeID,
		NodeName:  nodeName,
		NodeType:  nodeType,
		Status:    "RUNNING",
		StartTime: &nodeStart,
		ExtraData: truncate(input, 2000),
	}
	// 故意不检查错误：写 trace 失败不应阻塞主流程
	t.db.Create(&node)

	output, err := exec()

	now := time.Now()
	dur := now.Sub(nodeStart).Milliseconds()
	status := "SUCCESS"
	var errMsg string
	if err != nil {
		status = "ERROR"
		errMsg = truncate(err.Error(), 1000)
	}

	// 用 Updates(map) 而非结构体，避免零值字段被忽略（如 duration_ms=0 是合法的）。
	t.db.Model(&entity.RagTraceNodeDO{}).Where("trace_id = ? AND node_id = ?", t.traceID, nodeID).Updates(map[string]interface{}{
		"status":        status,
		"error_message": errMsg,
		"end_time":      now,
		"duration_ms":   dur,
		"extra_data":    truncate(output, 2000),
	})
	return output, err
}

// FinishRun 在 Chat() 外层 defer 调用，为 run 更新终态、总耗时和最终答案摘要。
// 注意：answer 优先取 LLM 正式回答；若是 guidance 引导文案，编排层会显式把 guidance.Message 传进来，
// 这样排障时能直接看到是否走了引导分支。
func (t *TraceRecorder) FinishRun(answer string, err error) {
	if !t.enabled || t.db == nil {
		return
	}
	now := time.Now()
	dur := now.Sub(t.start).Milliseconds()
	status := "SUCCESS"
	var errMsg string
	if err != nil {
		status = "ERROR"
		errMsg = truncate(err.Error(), 1000)
	}

	t.db.Model(&entity.RagTraceRunDO{}).Where("trace_id = ?", t.traceID).Updates(map[string]interface{}{
		"status":        status,
		"error_message": errMsg,
		"end_time":      now,
		"duration_ms":   dur,
		"extra_data":    truncate(answer, 2000),
	})
}

// GetTraceID 返回当前 run 的 traceID；在 WithTraceContext 时被调用。
// 注意 StartRun 之前调用会拿到空串。
func (t *TraceRecorder) GetTraceID() string {
	return t.traceID
}

// truncate 按 rune 截断字符串（CJK 安全），并带截断提示。
// 用来限制 extra_data / error_message 的大小，避免占用过多 DB 空间。
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + fmt.Sprintf("...(%d chars truncated)", len(runes)-maxLen)
}
