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

func NewTraceRecorder(db *gorm.DB, enabled bool) *TraceRecorder {
	return &TraceRecorder{db: db, enabled: enabled}
}

// StartRun 写入一条 RUNNING 的 run 记录，并初始化 traceID/runID/start。
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

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + fmt.Sprintf("...(%d chars truncated)", len(runes)-maxLen)
}

// RecordNode 记录某个阶段（node）：先插一条 RUNNING，再执行 exec，再按结果更新终态。
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

func (t *TraceRecorder) GetTraceID() string {
	return t.traceID
}
