package service

import "context"

// ========================= 设计目的 =========================
// RAG 链路追踪需要在一次请求的全生命周期里保持同一个 traceID（对应 t_rag_trace_run.trace_id）。
// 编排函数 orchestrator.RAGChatService.Chat 在 StartRun 之后调用 WithTraceContext，
// 把 traceID / taskID 写入 context.Context；后续任意下游（包括通过 errgroup 衍生出来的
// 子 ctx）都能通过 TraceIDFromContext / TaskIDFromContext 读到同一把值，从而：
//   1) 在日志里打 traceID，方便排障；
//   2) 在 TraceRecorder.RecordNode 把 node 和 run 绑定到同一个 trace_id；
//   3) 在异步任务（例如 MCP 工具并发调用、标题生成）里也能回填链路信息。
//
// ========================= 为什么自定义 key 类型 =========================
// context.WithValue 的官方约定：key 必须是“非内置类型”，否则不同包之间可能相互覆盖。
// 这里用私有的 traceContextKey（未导出，外部无法构造同名 key）保证安全。

// traceContextKey 是只在本文件可见的非导出类型，用于 context.WithValue 的 key。
// 采用 string 基础类型 + 非导出命名，避免与其它包的 key 冲突。
type traceContextKey string

const (
	// traceIDKey 在 context 中存放本次 RAG 请求的链路 ID（TraceRecorder 生成）。
	traceIDKey traceContextKey = "trace_id"
	// taskIDKey 在 context 中存放 SSE 流式任务 ID（chat_handler 生成），
	// 用于分布式取消：接收到取消事件后可以找到该 ctx 对应的 CancelFunc。
	taskIDKey traceContextKey = "task_id"
)

// WithTraceContext 返回一个带有 traceID（必填）和可选 taskID 的新 context。
// 调用点：orchestrator.RAGChatService.Chat 中，StartRun 之后；
// 注意：空 taskID 时不写入，避免下游把空串误认为“存在 taskID”。
func WithTraceContext(ctx context.Context, traceID, taskID string) context.Context {
	ctx = context.WithValue(ctx, traceIDKey, traceID)
	if taskID != "" {
		ctx = context.WithValue(ctx, taskIDKey, taskID)
	}
	return ctx
}

// TraceIDFromContext 从 context 读取本次请求的 traceID。
// 如果 ctx 中未曾写入（例如测试场景、异步任务脱离主 ctx），返回空串，由调用方决定降级策略。
func TraceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey).(string)
	return v
}

// TaskIDFromContext 从 context 读取 SSE 流式任务 ID，未设置时返回空串。
// 非流式 /chat 接口不会写入 taskID，此处返回空串是正常现象。
func TaskIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(taskIDKey).(string)
	return v
}
