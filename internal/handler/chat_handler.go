package handler

import (
	"context"
	"fmt"
	"gogent/internal/auth"
	"gogent/internal/chat"
	"gogent/internal/orchestrator"
	"gogent/internal/service"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

//RAG 聊天 HTTP 入口 —— SSE 流式为主，附停止流与旧版 JSON 接口。
//
// ========================= 路由 =========================
// GET  /rag/v3/chat  ：主流式接口（SSE）；meta → message*(think/response) → finish/cancel/reject → done
// POST /rag/v3/stop  ：按 taskId 触发取消（本地 + 分布式）
// POST /chat         ：旧版 JSON 接口（向后兼容，不走 SSE）
//
// ========================= 流式 handler 必须做的几件事 =========================
//   1) 生成 taskID 并 Register 到 StreamTaskManager；defer Unregister 防泄漏
//   2) 设置 SSE 头 + Flush，关闭反向代理缓冲（X-Accel-Buffering: no）
//   3) 发 meta 事件，立即告诉前端 conversationId/taskId
//   4) 首会话时准备 fallbackTitle + 触发异步 LLM 标题生成
//   5) 可选：按用户加 Redis SetNX 并发锁（同一用户同时只允许一个流式会话）
//   6) 可选：全局限流 Acquire / Release
//   7) 调 RAGChatService.Chat，传入 StreamBinder（把 stop 回调绑到 taskID）
//   8) 处理 guidance/stream/非流式 3 类结果分别发事件
//   9) 持久化 assistant 消息（即便 cancel，也要保留一条消息让前端定位反馈）
//  10) 发 finish/cancel + done；error 后只发 done（前端依赖 error 帧保留错误状态）

// ChatHandler 聚合聊天 HTTP 接口需要的依赖。
type ChatHandler struct {
	ragSvc  *orchestrator.RAGChatService
	db      *gorm.DB
	limiter *service.RateLimiter
	rdb     *redis.Client
}

func NewChatHandler(ragSvc *orchestrator.RAGChatService, db *gorm.DB, limiter *service.RateLimiter, rdb *redis.Client) *ChatHandler {
	return &ChatHandler{ragSvc: ragSvc, db: db, limiter: limiter, rdb: rdb}
}

func (h *ChatHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/rag/v3/chat", h.handleStreamChat)
	rg.POST("/rag/v3/stop", h.handleStop)
	rg.POST("/chat", h.handleChat)
}

// 这是整个 RAG 的主入口，SSE 流式返回。
func (h *ChatHandler) handleStreamChat(c *gin.Context) {
	// ---- 0. 参数解析与校验 ----
	question := c.Query("question")
	if question == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "A000001", "message": "question is required"})
		return
	}
	conversationID := c.Query("conversationId")
	// deepThinking=true 时传到 LLM 层开启思考链路（对应 qwen / doubao 等模型的 thinking 开关）
	deepThinking := c.DefaultQuery("deepThinking", "false") == "true"
	userID := auth.GetUserID(c.Request.Context())
	taskID := idgen.NextIDStr()
	// 新会话：前端没带 conversationId 时本处生成一个，并随 meta 下发
	if conversationID == "" {
		conversationID = idgen.NextIDStr()
	}
	// ---- 1. 创建可取消 ctx 并注册到 TaskManager ----
	// ctx 源于 Request，客户端断连时会自动 cancel；defer cancel 保证任何退出路径都释放
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	taskMgr := GetTaskManager()
	taskMgr.Register(taskID, cancel)
	defer taskMgr.Unregister(taskID)
	// ---- 2. 配置 SSE 响应头 ----
	c.Header("Content-Type", "text/event-stream;charset=UTF-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	// Nginx 默认会缓存响应体导致首字节延迟，这里强制关闭反向代理缓冲
	c.Header("X-Accel-Buffering", "no")

	// writeEvent：封装 SSE 行协议，每次 Flush 保证事件实时下发
	writeEvent := func(event, data string) {
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		c.Writer.Flush()
	}
	// ---- 3. 首帧 meta：告诉前端 taskId/conversationId ----
	writeEvent(EventMeta, sseJSON(MetaPayload{
		ConversationID: conversationID,
		TaskID:         taskID,
	}))
	// ---- 4. 确保会话记录存在，并准备兜底 Title ----
	conv := service.FindOrCreateConversation(h.db, conversationID, userID)
	// 首次会话：title 为空 → 用 fallback 临时占位，同时异步让 LLM 生成更好的标题
	isNewConversation := conv != nil && conv.Title == ""
	defaultTitle := "新对话"
	if isNewConversation {
		defaultTitle = fallbackTitle(question, 30)
		service.GenerateTitleAsync(h.db, h.ragSvc.GetLLMService(), conversationID, question)
	}
	// ---- 5. 按用户加并发锁：同一用户不允许同时开两个流式会话 ----
	if h.rdb != nil && userID != "" {
		userLockKey := "ragent:chat:user:lock"
		acquired, lockErr := h.rdb.SetNX(ctx, userLockKey, taskID, 60*time.Second).Result()
		if lockErr == nil {
			if !acquired {
				// 已有并发会话：发 reject + done 结束流
				rejectMsg := "当前会话处理中，请稍后再试"
				writeEvent(EventReject, sseJSON(MessageDelta{Type: "response", Delta: rejectMsg}))
				writeEvent(EventDone, "\"[DONE]\"")
				return
			}
			// Background ctx 解锁：即便请求 ctx 已 cancel，也要归还锁
			defer h.rdb.Del(context.Background(), userLockKey)
		}
	}
	// ---- 6. 全局限流 / 排队 ----
	var leaseID string
	if h.limiter != nil {
		if l, err := h.limiter.Acquire(ctx, userID); err == nil {
			leaseID = l
			defer h.limiter.Release(context.Background(), leaseID)
		} else {
			// 被限流：持久化“系统繁忙”消息，让前端仍能拿到 messageId 用于反馈
			rejectMsg := "系统繁忙，请稍后再试"
			var msgID string
			memorySvc := h.ragSvc.GetMemoryService()
			if memorySvc != nil {
				// memory 可用时走 memory（会同时维护缓存 + DB）
				_, _ = memorySvc.Append(context.Background(), conversationID, chat.Message{Role: "user", Content: question})
				id, _ := memorySvc.Append(context.Background(), conversationID, chat.Message{Role: "assistant", Content: rejectMsg})
				msgID = id
			} else {
				// 兜底直接写 DB，至少保证 assistant 消息有 ID
				_ = service.SaveConversationMessage(h.db, conversationID, userID, "user", question)
				msgID = service.SaveConversationMessage(h.db, conversationID, userID, "assistant", rejectMsg)
			}

			title := service.ResolveTitle(h.db, conversationID, defaultTitle)

			writeEvent(EventReject, sseJSON(MessageDelta{Type: "response", Delta: rejectMsg}))
			writeEvent(EventFinish, sseJSON(CompletionPayload{MessageID: msgID, Title: title}))
			writeEvent(EventDone, "\"[DONE]\"")
			return
		}
	}
	// ---- 7. 调 RAG 编排 ----
	// StreamBinder 的 lambda 会在编排层拿到 LLM 流后被调用，把 stop 关联到 taskID：
	// 后续 TaskManager.Cancel 不仅能 cancel ctx，还能直接关 LLM 连接
	result, err := h.ragSvc.Chat(ctx, orchestrator.ChatRequest{
		ConversationID: conversationID,
		UserID:         userID,
		Query:          question,
		Stream:         true,
		DeepThinking:   deepThinking,
		TaskID:         taskID,
		StreamBinder: orchestrator.StreamHandleBinderFunc(func(tid string, stop func()) {
			GetTaskManager().BindHandle(tid, stop)
		}),
	})
	if err != nil {
		// 编排阶段硬错（DB/Redis 挂了等）：发 error + done
		slog.Error("chat error", "err", err)
		writeEvent(EventError, sseJSON(ErrorPayload{Error: err.Error()}))
		writeEvent(EventDone, "\"[DONE]\"")
		return
	}

	// ---- 8a. guidance 分支：意图歧义时，编排层返回引导问题 ----
	if result.IsGuidance {
		// 走 reject（前端按"被拒"样式展示）+ finish 让消息终态化
		writeEvent(EventReject, sseJSON(MessageDelta{Type: "response", Delta: result.GuidanceMsg}))
		title := service.ResolveTitle(h.db, conversationID, defaultTitle)
		writeEvent(EventFinish, sseJSON(CompletionPayload{MessageID: result.MessageID, Title: title}))
		writeEvent(EventDone, "\"[DONE]\"")
		return
	}
	// ---- 8b. 流式正常分支：消费 StreamCh 并转发到 SSE ----
	var fullAnswer strings.Builder
	hadStreamError := false
	if result.StreamCh != nil {
		for delta := range result.StreamCh {
			if delta.Err != nil {
				// 流中途错误：前端依赖 error 帧保留错误态，不发 finish/cancel
				slog.Warn("stream error", "err", delta.Err)
				writeEvent(EventError, sseJSON(ErrorPayload{Error: delta.Err.Error()}))
				hadStreamError = true
				break
			}
			if delta.IsThinking {
				// 深度思考链路：类型 think，前端折叠展示
				writeEvent(EventMessage, sseJSON(MessageDelta{
					Type:  "think",
					Delta: delta.Content,
				}))
			} else if delta.Content != "" {
				// 正文：累积到 fullAnswer 用于稍后持久化
				fullAnswer.WriteString(delta.Content)
				writeEvent(EventMessage, sseJSON(MessageDelta{
					Type:  "response",
					Delta: delta.Content,
				}))
			}
		}
	} else if result.Answer != "" {
		// 非流式回退（例如 emptyRetrievalAnswer 直接塞 Answer 的情况）
		fullAnswer.WriteString(result.Answer)
		writeEvent(EventMessage, sseJSON(MessageDelta{
			Type:  "response",
			Delta: result.Answer,
		}))
	}
	// ---- 9. 持久化 assistant 消息并解析最终 Title ----
	var messageID string
	answerText := fullAnswer.String()
	finalAnswer := answerText
	// 被取消时给正文打个“已停止生成”标记
	if ctx.Err() == context.Canceled {
		marker := "（已停止生成）"
		if strings.TrimSpace(finalAnswer) == "" {
			finalAnswer = marker
		} else if !strings.Contains(finalAnswer, marker) {
			finalAnswer = finalAnswer + "\n\n" + marker
		}
	}
	// 非空才写库；防止空回复产生无意义的 assistant 消息
	if strings.TrimSpace(finalAnswer) != "" {
		memorySvc := h.ragSvc.GetMemoryService()
		if memorySvc != nil {
			// 注意用 Request ctx（而非已 cancel 的本地 ctx），让写入能完整结束
			msgID, _ := memorySvc.Append(c.Request.Context(), conversationID, chat.Message{
				Role:    "assistant",
				Content: finalAnswer,
			})
			messageID = msgID
		} else {
			messageID = service.SaveConversationMessage(h.db, conversationID, userID, "assistant", finalAnswer)
		}
	}
	title := service.ResolveTitle(h.db, conversationID, defaultTitle)
	// ---- 10. 收尾事件 ----
	// 出错时只关闭流，避免前端把 finish 帧当成"错误已清除"而覆盖错误样式
	if hadStreamError {
		writeEvent(EventDone, "\"[DONE]\"")
		return
	}
	// cancel vs finish：让前端区分"主动停止"与"正常完成"，UI 侧图标不同
	if ctx.Err() == context.Canceled {
		writeEvent(EventCancel, sseJSON(CompletionPayload{MessageID: messageID, Title: title}))
	} else {
		writeEvent(EventFinish, sseJSON(CompletionPayload{MessageID: messageID, Title: title}))
	}
	writeEvent(EventDone, "\"[DONE]\"")
}

// fallbackTitle 从 question 截取前 maxChars 个 rune 作为占位标题。
// 对比 strings.TrimSpace 后的字节长度是错的（CJK 字符 3 字节），所以用 utf8.RuneCountInString。
func fallbackTitle(question string, maxChars int) string {
	s := strings.TrimSpace(question)
	if s == "" {
		return "新对话"
	}
	if maxChars <= 0 {
		maxChars = 30
	}
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars])
}

// handleStop 处理 POST /rag/v3/stop?taskId=
// 前端点"停止生成"时调用；内部通过 TaskManager 触发本地 + 分布式 cancel。
func (h *ChatHandler) handleStop(c *gin.Context) {
	taskID := c.Query("taskId")
	if taskID == "" {
		response.FailWithCode(c, errcode.ClientError, "taskId is required")
		return
	}
	GetTaskManager().Cancel(taskID)
	response.Success(c, nil)
}

// handleChat 处理旧版 POST /chat（非流式 JSON）。
// 保留兼容性，主流程迁到 /rag/v3/chat。
func (h *ChatHandler) handleChat(c *gin.Context) {
	var req struct {
		ConversationID string `json:"conversationId" binding:"required"`
		Query          string `json:"query" binding:"required"`
		UserID         string `json:"userId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, err)
		return
	}

	// 优先用 body 里显式 userID（旧接口有此用法），否则从 JWT 里取
	userID := req.UserID
	if userID == "" {
		userID = auth.GetUserID(c.Request.Context())
	}

	result, err := h.ragSvc.Chat(c.Request.Context(), orchestrator.ChatRequest{
		ConversationID: req.ConversationID,
		UserID:         userID,
		Query:          req.Query,
		Stream:         false,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}

	if result.IsGuidance {
		response.Success(c, gin.H{
			"type":    "guidance",
			"message": result.GuidanceMsg,
		})
		return
	}

	response.Success(c, gin.H{
		"type":   "answer",
		"answer": result.Answer,
	})
}
