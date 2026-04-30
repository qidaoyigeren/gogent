package handler

import "encoding/json"

// ========================= 线上 SSE 行协议 =========================
// 每个事件一行 "event: <name>" + "data: <json>" + 空行。
// chat_handler.writeEvent 统一封装 fprintf + Flush，这里只负责定义事件名与载荷结构体。
//
// ========================= 一次完整流的事件序列 =========================
//   meta                         ← 首事件：带出 conversationId 和 taskId（前端立即持久化，用于后续 stop）
//   message × N                  ← 主体：流式增量，MessageDelta.type = "response" 或 "think"
//   finish / cancel / reject     ← 终态：finish=正常完成，cancel=客户端/服务端主动取消，reject=并发/限流拒绝
//   error（可选，替代 finish）    ← 流中途错误，前端保留错误提示，不再期望 finish
//   done                         ← 永远最后一帧：data='"[DONE]"'，前端据此关闭 EventSource

const (
	EventMeta    = "meta"    // 首帧：下发 conversationId / taskId
	EventMessage = "message" // 主体：流式文本增量
	EventFinish  = "finish"  // 正常结束
	EventDone    = "done"    // 终止帧：关闭 EventSource
	EventCancel  = "cancel"  // 用户/服务端取消
	EventReject  = "reject"  // 被限流 / 并发锁 / 引导回绝
	EventTitle   = "title"   // 标题异步生成成功时下发（可选）
	EventError   = "error"   // 流级错误
)

// MetaPayload：meta 事件的 JSON body。
// 前端通常在这一帧就把 conversationId 持久化到 URL，后续 stop 接口通过 taskID 发起取消。
type MetaPayload struct {
	ConversationID string `json:"conversationId"`
	TaskID         string `json:"taskId"`
}

// MessageDelta：message 事件的 JSON body —— 一次流式增量。
// Type 两种取值：
//
//	"response" ：用户可见的答案文本，前端追加到 assistantMessage.content
//	"think"    ：深度思考链路（DeepThinking 开启时），前端单独展示折叠区块
type MessageDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// CompletionPayload：finish / cancel / reject 事件的 JSON body。
// MessageID 让前端知道当前 assistant 消息在 DB 的主键（反馈、会话回放均需要）。
// Title 在首次会话时回填，让前端侧边栏立刻显示有意义的标题。
type CompletionPayload struct {
	MessageID string `json:"messageId,omitempty"`
	Title     string `json:"title,omitempty"`
}

// ErrorPayload：error 事件 body。
// 字段名必须是 "error"（小写），与前端 useStreamResponse.ts 强耦合。
type ErrorPayload struct {
	Error string `json:"error"`
}

// sseJSON 便捷序列化；错误被吞（极少失败，且失败也无需中断流，返回空串即可）。
// 约束：调用方必须保证 v 可序列化（当前都是简单 struct，不含 chan / func）。
func sseJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
