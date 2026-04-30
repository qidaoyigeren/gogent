package orchestrator

//在线链路收口 — 本文件是 RAG 问答的编排核心。

import (
	"context"
	"fmt"
	"gogent/internal/chat"
	"gogent/internal/entity"
	"gogent/internal/guidance"
	"gogent/internal/intent"
	"gogent/internal/mcp"
	"gogent/internal/memory"
	"gogent/internal/prompt"
	"gogent/internal/retrieve"
	"gogent/internal/rewrite"
	"gogent/internal/service"
	"log/slog"
	"strings"

	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

// defaultRetrieveTopK：意图节点未显式指定 topK 时使用。对齐 Java RAGConstant.DEFAULT_TOP_K。
const (
	defaultRetrieveTopK = 10
)

// RAGChatService 是 RAG 问答的核心编排器
//
//	load memory + 持久化用户 turn → 问题重写 → 意图识别 →
//	引导（歧义）→ 仅系统意图短路 → 多路检索 → MCP 工具 → prompt 构建 → LLM 调用
//
// 依赖都是"接口或具体服务"，构造期注入；单实例使用。
type RAGChatService struct {
	llm             chat.LLMService                   // Day06：聊天模型
	memorySvc       *memory.ConversationMemoryService // Day11：记忆 + 历史
	rewriteSvc      rewrite.QueryRewriteService       // Day10：问题重写 + 拆分子问
	intentResolver  *intent.Resolver                  // Day10：意图打分
	intentCache     *intent.TreeCache                 // Day10：意图树缓存
	guidanceSvc     *guidance.Service                 // Day10：歧义引导决策
	guidancePending *guidance.ConversationPending     // Day10：会话级引导待选集
	retrieveEngine  *retrieve.MultiChannelEngine      // Day09：多路检索
	mcpRegistry     *mcp.Registry                     // Day12：MCP 工具注册表
	mcpExtractor    *mcp.ParamExtractor               // Day12：MCP 参数抽取
	promptSvc       *prompt.RAGPromptService          // Day06/11：prompt 构建
	db              *gorm.DB                          // Day02：trace 写入
	traceEnabled    bool                              // 全局 trace 开关
}

// NewRAGChatService 构造函数；main 里注入上述依赖。traceEnabled 固定为 true，
// 关闭 trace 可通过把 DB 传 nil 或未来扩展配置项实现。
func NewRAGChatService(
	llm chat.LLMService,
	memorySvc *memory.ConversationMemoryService,
	rewriteSvc rewrite.QueryRewriteService,
	intentResolver *intent.Resolver,
	intentCache *intent.TreeCache,
	guidanceSvc *guidance.Service,
	guidancePending *guidance.ConversationPending,
	retrieveEngine *retrieve.MultiChannelEngine,
	mcpRegistry *mcp.Registry,
	mcpExtractor *mcp.ParamExtractor,
	promptSvc *prompt.RAGPromptService,
	db *gorm.DB,
) *RAGChatService {
	return &RAGChatService{
		llm:             llm,
		memorySvc:       memorySvc,
		rewriteSvc:      rewriteSvc,
		intentResolver:  intentResolver,
		intentCache:     intentCache,
		guidanceSvc:     guidanceSvc,
		guidancePending: guidancePending,
		retrieveEngine:  retrieveEngine,
		mcpRegistry:     mcpRegistry,
		mcpExtractor:    mcpExtractor,
		promptSvc:       promptSvc,
		db:              db,
		traceEnabled:    true,
	}
}

// StreamHandleBinder 在 LLM 流开始后被调用，把“上游 stop 回调”绑定到 taskID。
// chat_handler 注入的实现会转发到 handler.StreamTaskManager.BindHandle。
// 对齐 Java StreamTaskManager#bindHandle。
type StreamHandleBinder interface {
	BindHandle(taskID string, stop func())
}

// StreamHandleBinderFunc 适配任意函数到 StreamHandleBinder 接口。
// 避免 handler 层专门为此写一个类型。
type StreamHandleBinderFunc func(taskID string, stop func())

func (f StreamHandleBinderFunc) BindHandle(taskID string, stop func()) {
	if f != nil {
		f(taskID, stop)
	}
}

// attachStreamCancelCleanup：把 LLM 返回的 stream chan 包装一层，
// 在 chan 关闭后自动调用 cancel，防止子 ctx 泄漏（go vet: lostcancel）。
// cancel 是幂等的：BindHandle 注册后外部也可能触发一次。
// 非 nil 输出保证与输入同阶顺序（不会打乱事件）。
func attachStreamCancelCleanup(ch <-chan chat.StreamDelta, cancel context.CancelFunc) <-chan chat.StreamDelta {
	if cancel == nil {
		return ch
	}
	if ch == nil {
		// 输入空：立刻 cancel 释放资源
		cancel()
		return nil
	}
	out := make(chan chat.StreamDelta)
	go func() {
		defer close(out)
		defer cancel() // 无论成功/失败都保证 cancel
		for d := range ch {
			out <- d
		}
	}()
	return out
}

// ChatRequest 一次问答的入参。
// TaskID / StreamBinder 仅在 Stream=true 时需要（非流式无从 stop）。
type ChatRequest struct {
	ConversationID string
	UserID         string
	Query          string
	Stream         bool
	DeepThinking   bool
	TaskID         string             // SSE 流式任务 ID；会写入 trace_run.task_id
	StreamBinder   StreamHandleBinder // 用来把 LLM stop 回调注册到 TaskManager
}

// ChatResult 一次问答的出参（三种互斥分支）：
//   - 正常流式：StreamCh 非空，handler 遍历消费
//   - 非流式 / 兜底：Answer 非空
//   - 引导：IsGuidance=true + GuidanceMsg 非空
type ChatResult struct {
	Answer      string
	GuidanceMsg string
	IsGuidance  bool
	StreamCh    <-chan chat.StreamDelta
	MessageID   string // 持久化后的 assistant 消息 ID
	Title       string
}

// Chat 是对外唯一入口；只负责 trace 外层生命周期，具体步骤委托 doChat。
// 不论 doChat 是否 panic/error，FinishRun 都会写入 run 的终态（err 同步序列化到 error_message）。
func (s *RAGChatService) Chat(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	// 1) 初始化 TraceRecorder 并 StartRun（若 traceEnabled=false 或 db=nil，recorder 所有方法都 no-op）
	tracer := service.NewTraceRecorder(s.db, s.traceEnabled)
	tracer.StartRun(req.ConversationID, req.TaskID, req.UserID, req.Query)
	ctx = service.WithTraceContext(ctx, tracer.GetTraceID(), req.TaskID)

	// 2) 真正的编排
	result, err := s.doChat(ctx, req, tracer)

	// 3) FinishRun：优先用 guidance 文案作为"答案摘要"，方便排障时看出走了哪条分支
	var answer string
	if result != nil {
		answer = result.Answer
		if result.GuidanceMsg != "" {
			answer = result.GuidanceMsg
		}
	}
	tracer.FinishRun(answer, err)

	return result, err
}

// doChat 串起所有阶段；每个阶段都通过 tracer.RecordNode 包一层。
// 为保持可读性，各阶段顺序与 Java 版本 RAGChatServiceImpl 严格对齐。
func (s *RAGChatService) doChat(ctx context.Context, req ChatRequest, tracer *service.TraceRecorder) (*ChatResult, error) {
	// ========================= 1. 加载记忆 =========================
	var memCtx *memory.MemoryContext
	tracer.RecordNode("memory-load", "MEMORY", req.ConversationID, func() (string, error) {
		mc, err := s.loadMemory(ctx, req.ConversationID)
		if err != nil {
			// 降级：记忆读失败不终止主流程，用空上下文继续
			slog.Warn("memory load failed", "err", err)
			mc = &memory.MemoryContext{}
		}
		memCtx = mc
		return fmt.Sprintf("history=%d", len(mc.History)), nil
	})

	// 立即把当前 user turn 追加进 memory + DB。
	// 时机与 Java loadAndAppend 对齐：即便后续阶段失败，用户这条问题也已经持久化。
	userTurn := chat.Message{Role: "user", Content: req.Query}
	if _, err := s.appendMemory(ctx, req.ConversationID, userTurn); err != nil {
		slog.Warn("memory append user failed", "err", err)
	}
	memCtx.History = append(memCtx.History, userTurn)

	// ========================= 2. 问题重写 =========================
	var rewriteResult *rewrite.RewriteResult
	tracer.RecordNode("query-rewrite", "REWRITE", req.Query, func() (string, error) {
		rr, err := s.rewriteSvc.RewriteMulti(ctx, req.Query, memCtx.History)
		if err != nil {
			// 降级：重写失败时直接用原问题
			slog.Warn("rewrite failed", "err", err)
			rr = &rewrite.RewriteResult{Rewritten: req.Query}
		}
		rewriteResult = rr
		return fmt.Sprintf("rewritten=%s, subQuestions=%d", rr.Rewritten, len(rr.SubQuestions)), nil
	})

	slog.Info("query rewritten", "original", req.Query, "rewritten", rewriteResult.Rewritten,
		"subQuestions", len(rewriteResult.SubQuestions))

	// ========================= 3. 意图识别 =========================
	var intentGroup *intent.IntentGroup
	tracer.RecordNode("intent-resolve", "INTENT", rewriteResult.Rewritten, func() (string, error) {
		roots, _ := s.intentCache.GetRoots(ctx)
		// 子问题可能为空：兜底用 rewritten 作为唯一子问
		subQuestions := rewriteResult.SubQuestions
		if len(subQuestions) == 0 {
			subQuestions = []string{rewriteResult.Rewritten}
		}

		ig, err := s.intentResolver.Resolve(ctx, subQuestions, roots)
		if err != nil {
			slog.Warn("intent resolution failed", "err", err)
			ig = &intent.IntentGroup{}
		}
		intentGroup = ig
		return fmt.Sprintf("subQuestions=%d, topScore=%.2f", len(ig.SubQuestions), ig.TopScore()), nil
	})

	// ========================= 4–5. 歧义引导与仅系统意图短路 =========================
	// 注意：这里的顺序与 Java RAGChatServiceImpl 严格一致：先处理歧义/引导，再判定仅系统。
	roots, _ := s.intentCache.GetRoots(ctx)

	// 若用户上一轮收到"1/2/3"引导后，本轮只输入数字，尝试把选择转成强制 intent
	if s.tryResolveGuidanceChoice(ctx, req, memCtx, &intentGroup, roots) {
		slog.Info("guidance digit choice applied", "conversationId", req.ConversationID, "choice", strings.TrimSpace(req.Query))
	}
	nodeMap := buildNodeMap(roots)

	// 引导决策：若需要再让用户二选一，直接返回 guidance 消息并落 memory
	decision := s.guidanceSvc.Evaluate(intentGroup, nodeMap)
	if decision.Type == guidance.DecisionGuide {
		// 把本轮候选记到 pending，下一轮用户回复数字时能解析
		if s.guidancePending != nil && len(decision.OptionNodeIDs) > 0 {
			s.guidancePending.Set(req.ConversationID, decision.OptionNodeIDs)
		}
		var msgID string
		tracer.RecordNode("guidance", "GUIDANCE", "", func() (string, error) {
			id, _ := s.appendMemory(ctx, req.ConversationID, chat.Message{Role: "assistant", Content: decision.Message})
			msgID = id
			return decision.Message, nil
		})
		return &ChatResult{
			GuidanceMsg: decision.Message,
			IsGuidance:  true,
			MessageID:   msgID,
		}, nil
	}

	// 仅系统意图（不需要检索知识库的纯系统问答）：跳过 retrieve/MCP，直接按 system prompt 走
	allSystemOnly := s.checkAllSystemOnly(intentGroup, nodeMap)
	if allSystemOnly {
		customPrompt := s.findSystemPromptTemplate(intentGroup, nodeMap)
		result, err := s.streamSystemResponse(ctx, req, rewriteResult.Rewritten, memCtx, customPrompt, tracer)
		return result, err
	}

	// ========================= 6. 构建每个子问的执行计划 =========================
	// plan = 一个子问题 + 它要查的 KB 列表 + 要调的 MCP 工具 + topK
	plans := buildSubQuestionPlans(intentGroup, nodeMap, rewriteResult.Rewritten)

	// ========================= 7. 多路检索（每个子问独立并发） =========================
	var chunks []retrieve.DocumentChunk
	tracer.RecordNode("retrieval", "RETRIEVE", fmt.Sprintf("subQuestions=%d", len(plans)), func() (string, error) {
		results := make([][]retrieve.DocumentChunk, len(plans))
		// errgroup：任一 goroutine 返回 err 会 cancel gCtx；其它 goroutine 能尽快退出
		g, gCtx := errgroup.WithContext(ctx)
		for i, plan := range plans {
			i, plan := i, plan // 闭包变量捕获陷阱保护
			g.Go(func() error {
				scoreCount, maxScore := intentScoreStatsForSubQuestion(intentGroup, plan.Question)
				retrieveCtx := &retrieve.RetrievalContext{
					Query:            plan.Question,
					IntentKBIDs:      plan.KBIDs,
					IntentKBMaxScore: cloneFloatMap(plan.KBMaxScore), // 防止并发写
					TopK:             resolvePlanTopK(plan.TopK),
					UserID:           req.UserID,
					IntentScoreCount: scoreCount,
					MaxIntentScore:   maxScore,
				}
				ch, err := s.retrieveEngine.Retrieve(gCtx, retrieveCtx)
				if err != nil {
					slog.Warn("retrieval failed", "question", plan.Question, "err", err)
					return err
				}
				// 把 sub-question 写入 metadata：prompt formatter 按子问分段展示
				for j := range ch {
					if ch[j].Metadata == nil {
						ch[j].Metadata = map[string]string{}
					}
					ch[j].Metadata["subQuestion"] = plan.Question
				}
				results[i] = ch
				return nil
			})
		}
		err := g.Wait()
		// 并发结果按子问顺序合并成总 chunks（顺序稳定）
		for _, part := range results {
			chunks = append(chunks, part...)
		}
		return fmt.Sprintf("chunks=%d", len(chunks)), err
	})

	// 把所有子问的 MCP 调用展平，准备统一并发
	mcpCalls := flattenMCPCalls(plans)

	// 判断是否"所有子问都没有任何 KB / MCP 目标"（比如寒暄、领域外问题）
	allPlansLackTargets := true
	for _, p := range plans {
		if len(p.KBIDs) > 0 || len(p.MCPCalls) > 0 {
			allPlansLackTargets = false
			break
		}
	}
	// 完全没有任何检索目标 → 用通用助手 system prompt 回答，避免空空回复"检索不到"
	if len(chunks) == 0 && len(mcpCalls) == 0 && allPlansLackTargets {
		return s.streamSystemResponse(ctx, req, rewriteResult.Rewritten, memCtx, "", tracer)
	}

	// 有目标但都没检到 → Java 的固定兜底回复
	if len(chunks) == 0 && len(mcpCalls) == 0 {
		return s.emptyRetrievalAnswer(ctx, req)
	}

	// ========================= 8. MCP 工具并发调用 =========================
	var mcpResults []*mcp.MCPResponse
	if len(mcpCalls) > 0 {
		tracer.RecordNode("mcp-tools", "MCP", fmt.Sprintf("calls=%d", len(mcpCalls)), func() (string, error) {
			parallelRes := s.runMCPToolsParallel(ctx, req.UserID, mcpCalls)
			var totalCostMs int64
			var okCount, failCount int
			mcpResults = make([]*mcp.MCPResponse, 0, len(mcpCalls))
			for i, call := range mcpCalls {
				toolID := call.ToolID
				resp := parallelRes[i]
				// 每个工具再包一个 RecordNode，粒度更细，便于发现"哪个工具慢/失败"
				_, _ = tracer.RecordNode("mcp-tool", "MCP_TOOL", toolID, func() (string, error) {
					if resp == nil {
						failCount++
						return "nil-response", nil
					}
					mcpResults = append(mcpResults, resp)
					totalCostMs += resp.CostMs
					if resp.Success {
						okCount++
						return fmt.Sprintf("success costMs=%d", resp.CostMs), nil
					}
					failCount++
					return fmt.Sprintf("failed code=%s costMs=%d", resp.ErrorCode, resp.CostMs), nil
				})
			}
			return fmt.Sprintf("results=%d ok=%d fail=%d totalCostMs=%d", len(mcpResults), okCount, failCount, totalCostMs), nil
		})
	}

	// 没 KB 命中 且 MCP 全失败 → 走空回退
	if len(chunks) == 0 && !mcpResultsHaveSuccess(mcpResults) {
		return s.emptyRetrievalAnswer(ctx, req)
	}

	// ========================= 9. 构建 prompt =========================
	// 注意：history 要排除当前 user turn（已在 BuildPrompt 里用 rewritten 重新拼成最后一轮 user message）
	intentTemplate := collectIntentPromptTemplate(intentGroup, nodeMap)
	messages := s.promptSvc.BuildPrompt(
		rewriteResult.Rewritten,
		rewriteResult.SubQuestions,
		memCtx.Summary,
		chunks,
		mcpResults,
		intentTemplate,
		historyPriorToCurrentUser(memCtx.History),
	)

	// ========================= 10. 动态温度 =========================
	// 无 MCP：temperature=0（严格基于检索文本回答）；
	// 有成功 MCP：temperature=0.3 + top_p=0.8（给工具结果做综合表述留点灵活度）
	temperature := 0.0
	topP := 1.0
	if mcpResultsHaveSuccess(mcpResults) {
		temperature = 0.3
		topP = 0.8
	}

	// ========================= 11. LLM 调用 =========================
	if req.Stream {
		// 流式：专门开一个子 ctx，BindHandle 注册 stop 回调到 TaskManager
		var streamCh <-chan chat.StreamDelta
		tracer.RecordNode("llm-stream", "LLM", fmt.Sprintf("temperature=%.1f", temperature), func() (string, error) {
			llmCtx, llmCancel := context.WithCancel(ctx)
			if req.TaskID != "" && req.StreamBinder != nil {
				req.StreamBinder.BindHandle(req.TaskID, llmCancel)
			}
			ch, err := s.llm.ChatStream(llmCtx, messages, chat.WithTemperature(temperature), chat.WithTopP(topP), chat.WithThinking(req.DeepThinking))
			if err != nil {
				// 启动失败立即释放 ctx，防止泄漏
				llmCancel()
				return "", err
			}
			// 包一层 cleanup：chan 关闭时自动 cancel；避免 go vet lostcancel
			streamCh = attachStreamCancelCleanup(ch, llmCancel)
			return "stream-started", nil
		})
		if streamCh == nil {
			return nil, fmt.Errorf("stream channel is nil")
		}

		return &ChatResult{StreamCh: streamCh}, nil
	}

	// 非流式：直接返回完整答案并持久化 assistant 消息
	var resp *chat.ChatResponse
	tracer.RecordNode("llm-call", "LLM", fmt.Sprintf("temperature=%.1f, messages=%d", temperature, len(messages)), func() (string, error) {
		r, err := s.llm.Chat(ctx, messages, chat.WithTemperature(temperature), chat.WithTopP(topP), chat.WithThinking(req.DeepThinking))
		if err != nil {
			return "", err
		}
		resp = r
		return fmt.Sprintf("tokens=%d", r.Usage.TotalTokens), nil
	})
	if resp == nil {
		return nil, fmt.Errorf("LLM returned nil")
	}

	msgID, _ := s.appendMemory(ctx, req.ConversationID, chat.Message{Role: "assistant", Content: resp.Content})

	return &ChatResult{Answer: resp.Content, MessageID: msgID}, nil
}

// GetMemoryService 暴露 memory 服务给 chat_handler，用于拒答/限流场景下的消息落库。
func (s *RAGChatService) GetMemoryService() *memory.ConversationMemoryService {
	return s.memorySvc
}

// GetLLMService 暴露 LLM 服务给 chat_handler，用于异步生成会话标题（GenerateTitleAsync）。
func (s *RAGChatService) GetLLMService() chat.LLMService {
	return s.llm
}

// checkAllSystemOnly 判断所有子问是否只命中单个 SYSTEM 类型意图。
// 成立条件：每个子问有且仅有 1 个 Score 且对应 node 的 Kind=SYSTEM。
// 为什么"单个 Score"：多候选意味着存在歧义，应走引导。
func (s *RAGChatService) checkAllSystemOnly(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode) bool {
	if len(group.SubQuestions) == 0 {
		return false
	}
	for _, sq := range group.SubQuestions {
		if len(sq.Scores) != 1 {
			return false
		}
		node, ok := nodeMap[sq.Scores[0].NodeID]
		if !ok || node == nil {
			return false
		}
		if !strings.EqualFold(node.Kind, entity.IntentKindSYSTEM) {
			return false
		}
	}
	return true
}

// findSystemPromptTemplate 取第一个非空的 PromptTemplate。
// 若意图节点配置了定制 system prompt，会覆盖通用"你是一个智能助手"。
func (s *RAGChatService) findSystemPromptTemplate(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode) string {
	for _, sq := range group.SubQuestions {
		for _, ns := range sq.Scores {
			if node, ok := nodeMap[ns.NodeID]; ok && node.PromptTemplate != "" {
				return node.PromptTemplate
			}
		}
	}
	return ""
}

// streamSystemResponse 走仅系统分支：不做检索 / 不用 MCP，直接把 system + history + user 送 LLM。
// 温度 0.7（更自然的对话感），deepThinking 固定关闭。
func (s *RAGChatService) streamSystemResponse(ctx context.Context, req ChatRequest, rewritten string,
	memCtx *memory.MemoryContext, customPrompt string, tracer *service.TraceRecorder) (*ChatResult, error) {

	systemPrompt := customPrompt
	if systemPrompt == "" {
		systemPrompt = "你是一个智能助手，请直接回答用户的问题。"
	}

	messages := []chat.Message{{Role: "system", Content: systemPrompt}}
	prior := historyPriorToCurrentUser(memCtx.History)
	if len(prior) > 0 {
		messages = append(messages, prior...)
	}
	messages = append(messages, chat.Message{Role: "user", Content: rewritten})

	temperature := 0.7

	if req.Stream {
		var streamCh <-chan chat.StreamDelta
		tracer.RecordNode("llm-system-stream", "LLM", fmt.Sprintf("temperature=%.1f, system-only", temperature), func() (string, error) {
			llmCtx, llmCancel := context.WithCancel(ctx)
			if req.TaskID != "" && req.StreamBinder != nil {
				req.StreamBinder.BindHandle(req.TaskID, llmCancel)
			}
			ch, err := s.llm.ChatStream(llmCtx, messages, chat.WithTemperature(temperature), chat.WithThinking(false))
			if err != nil {
				llmCancel()
				return "", err
			}
			streamCh = attachStreamCancelCleanup(ch, llmCancel)
			return "stream-started", nil
		})
		if streamCh == nil {
			return nil, fmt.Errorf("stream channel is nil")
		}
		return &ChatResult{StreamCh: streamCh}, nil
	}

	var resp *chat.ChatResponse
	tracer.RecordNode("llm-system-call", "LLM", fmt.Sprintf("temperature=%.1f, system-only", temperature), func() (string, error) {
		r, err := s.llm.Chat(ctx, messages, chat.WithTemperature(temperature), chat.WithThinking(false))
		if err != nil {
			return "", err
		}
		resp = r
		return resp.Content, nil
	})
	if resp == nil {
		return nil, fmt.Errorf("LLM returned nil")
	}

	msgID, _ := s.appendMemory(ctx, req.ConversationID, chat.Message{Role: "assistant", Content: resp.Content})
	return &ChatResult{Answer: resp.Content, MessageID: msgID}, nil
}

// historyPriorToCurrentUser 去掉历史末尾的"当前 user turn"。
// 由于 doChat 在前面 loadAndAppend 时已经把用户这条问题塞进 memCtx.History，
// BuildPrompt 会再用 rewritten 重新生成一轮 user message，因此要剔除末尾避免重复。
func historyPriorToCurrentUser(h []chat.Message) []chat.Message {
	n := len(h)
	if n > 0 && strings.EqualFold(h[n-1].Role, "user") {
		return h[:n-1]
	}
	return h
}

// intentScoreStatsForSubQuestion 统计某个子问对应的 score 数量 + 最大分。
// 用于 retrieve 中判断是否"意图足够明确"（影响阈值过滤策略）。
func intentScoreStatsForSubQuestion(group *intent.IntentGroup, question string) (count int, maxScore float64) {
	q := strings.TrimSpace(question)
	for _, sq := range group.SubQuestions {
		if strings.TrimSpace(sq.Question) != q {
			continue
		}
		for _, ns := range sq.Scores {
			count++
			if ns.Score > maxScore {
				maxScore = ns.Score
			}
		}
		return count, maxScore
	}
	return 0, 0
}

// buildNodeMap 把意图树 roots 展平成 id → node 的查表结构，O(1) 查询。
func buildNodeMap(roots []*intent.IntentNode) map[string]*intent.IntentNode {
	m := make(map[string]*intent.IntentNode)
	for _, root := range roots {
		for _, node := range root.FlattenAll() {
			m[node.ID] = node
		}
	}
	return m
}

// collectKBIDs 已不再被 doChat 直接调用（改用 buildSubQuestionPlans）；
// 保留以兼容其他调用方，按 minScore 汇总所有符合条件的 KB ID 去重列表。
func collectKBIDs(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode, minScore float64) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, sq := range group.SubQuestions {
		for _, ns := range sq.Scores {
			if ns.Score < minScore {
				continue
			}
			node, ok := nodeMap[ns.NodeID]
			if !ok || node == nil {
				continue
			}
			// Java 兼容：kind 为空或 KB 类型都接受
			if node.Kind != "" && !strings.EqualFold(node.Kind, entity.IntentKindKB) {
				continue
			}
			for _, kbID := range node.KBIDs {
				if !seen[kbID] {
					seen[kbID] = true
					ids = append(ids, kbID)
				}
			}
		}
	}
	return ids
}

// collectIntentPromptTemplate 在命中的意图节点里挑出分数最高的 PromptTemplate。
// 最终拼装 prompt 时会作为"意图专属系统提示"叠加进系统消息。
func collectIntentPromptTemplate(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode) string {
	bestScore := -1.0
	best := ""
	for _, sq := range group.SubQuestions {
		for _, ns := range sq.Scores {
			node, ok := nodeMap[ns.NodeID]
			if !ok || node == nil {
				continue
			}
			tpl := strings.TrimSpace(node.PromptTemplate)
			if tpl == "" {
				continue
			}
			if ns.Score > bestScore {
				bestScore = ns.Score
				best = tpl
			}
		}
	}
	return best
}

// mcpCallSpec 单次 MCP 工具调用的描述：哪个工具、来自哪个子问、用哪个 prompt 抽取参数。
type mcpCallSpec struct {
	ToolID              string
	Question            string
	ParamPromptTemplate string
}

// subQuestionPlan 单个子问的"执行计划"。
// KBMaxScore：每个 KB 的最高分，用于检索层对"强意图"走更宽容的阈值。
type subQuestionPlan struct {
	Question   string
	KBIDs      []string
	KBMaxScore map[string]float64
	TopK       int
	MCPCalls   []mcpCallSpec
}

// cloneFloatMap 浅拷贝 map[string]float64，避免并发 retrieve goroutine 修改同一 map。
func cloneFloatMap(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// buildSubQuestionPlans 把 IntentGroup + node 元数据组合成每子问的执行计划。
// 规则：
//   - 分数低于 IntentMinScore 的 Score 直接跳过
//   - KB 和 MCP 两条分支并行处理，不互相短路（Java filterKbIntents/filterMCPIntents 对齐）
//   - TopK 取该子问所有命中节点里最大的一个（更强意图覆盖更大召回窗口）
//   - 完全无 plan 时用 fallbackQuestion（通常是 rewrite 后的原问题）兜底
func buildSubQuestionPlans(group *intent.IntentGroup, nodeMap map[string]*intent.IntentNode, fallbackQuestion string) []subQuestionPlan {
	plans := make([]subQuestionPlan, 0, len(group.SubQuestions))
	for _, sq := range group.SubQuestions {
		question := strings.TrimSpace(sq.Question)
		if question == "" {
			question = fallbackQuestion
		}
		if question == "" {
			continue
		}
		plan := subQuestionPlan{Question: question}
		seenKB := make(map[string]bool)
		maxTopK := 0
		for _, ns := range sq.Scores {
			if ns.Score < intent.IntentMinScore {
				continue
			}
			node, ok := nodeMap[ns.NodeID]
			if !ok || node == nil {
				continue
			}
			// KB 分支：kind 为空或明确标为 KB 都处理（兼容历史数据）
			if node.Kind == "" || strings.EqualFold(node.Kind, entity.IntentKindKB) {
				if node.TopK != nil && *node.TopK > maxTopK {
					maxTopK = *node.TopK
				}
				for _, kbID := range node.KBIDs {
					kbID = strings.TrimSpace(kbID)
					if kbID == "" || seenKB[kbID] {
						continue
					}
					seenKB[kbID] = true
					plan.KBIDs = append(plan.KBIDs, kbID)
					// 记录 KB 上的最大 classifier score：retrieve 层可按分数做"信心加权"
					if plan.KBMaxScore == nil {
						plan.KBMaxScore = make(map[string]float64)
					}
					if prev, ok := plan.KBMaxScore[kbID]; !ok || ns.Score > prev {
						plan.KBMaxScore[kbID] = ns.Score
					}
				}
			}
			// MCP 分支：追加工具调用
			if strings.EqualFold(node.Kind, entity.IntentKindMCP) {
				for _, toolID := range node.MCPTools {
					toolID = strings.TrimSpace(toolID)
					if toolID == "" {
						continue
					}
					plan.MCPCalls = append(plan.MCPCalls, mcpCallSpec{
						ToolID:              toolID,
						Question:            question,
						ParamPromptTemplate: strings.TrimSpace(node.ParamPromptTemplate),
					})
				}
			}
		}
		if maxTopK > 0 {
			plan.TopK = maxTopK
		}
		plans = append(plans, plan)
	}

	// 空 plans 兜底：让后续 retrieve 至少知道用户问了啥，不会静默跳过
	if len(plans) == 0 && strings.TrimSpace(fallbackQuestion) != "" {
		plans = append(plans, subQuestionPlan{Question: strings.TrimSpace(fallbackQuestion)})
	}
	return plans
}

// flattenMCPCalls 把各 plan 里的 MCP 调用汇总成一个一维切片，
// 方便在 runMCPToolsParallel 里用 errgroup 统一并发。
func flattenMCPCalls(plans []subQuestionPlan) []mcpCallSpec {
	var calls []mcpCallSpec
	for _, p := range plans {
		calls = append(calls, p.MCPCalls...)
	}
	return calls
}

// resolvePlanTopK 把 plan 的 topK 规整为"非零正数"，否则用默认。
func resolvePlanTopK(v int) int {
	if v > 0 {
		return v
	}
	return defaultRetrieveTopK
}

// mcpResultsHaveSuccess 任一工具成功即 true。
// 用于决定是否进入"有 MCP 上下文"的回答模式（更高 temperature）。
func mcpResultsHaveSuccess(resps []*mcp.MCPResponse) bool {
	for _, r := range resps {
		if r != nil && r.Success {
			return true
		}
	}
	return false
}

// emptyRetrievalAnswer 完全没有检索到任何有效上下文时的固定兜底回复。
// 流式分支直接返回 Answer（不走 stream），handler 会把它当作一次性消息发出去。
func (s *RAGChatService) emptyRetrievalAnswer(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	const emptyReply = "未检索到与问题相关的文档内容。"
	if req.Stream {
		return &ChatResult{Answer: emptyReply}, nil
	}
	msgID, _ := s.appendMemory(ctx, req.ConversationID, chat.Message{Role: "assistant", Content: emptyReply})
	return &ChatResult{Answer: emptyReply, MessageID: msgID}, nil
}

// loadMemory 薄封装，主要处理 memorySvc==nil 时的空返回（测试环境方便）。
func (s *RAGChatService) loadMemory(ctx context.Context, conversationID string) (*memory.MemoryContext, error) {
	if s.memorySvc == nil {
		return &memory.MemoryContext{}, nil
	}
	return s.memorySvc.Load(ctx, conversationID)
}

// tryResolveGuidanceChoice 处理引导场景的数字回复（"1"/"2"/...）。
// 成功识别时：
//   - 用户真实意图是"选择上轮引导里的第 idx 个节点"
//   - 重新用 priorUser（两轮前真正的问题）做一次 intent resolve 填 scores，并强制选中的节点为高分
//   - 清空 pending
//
// 为什么判断 history 末 3 条：确保上一条 assistant 是引导提示、当前 user 是单个数字。
func (s *RAGChatService) tryResolveGuidanceChoice(ctx context.Context, req ChatRequest, memCtx *memory.MemoryContext, intentGroup **intent.IntentGroup, roots []*intent.IntentNode) bool {
	if intentGroup == nil || *intentGroup == nil || s.guidancePending == nil {
		return false
	}
	q := strings.TrimSpace(req.Query)
	if len(q) != 1 || q[0] < '1' || q[0] > '9' {
		return false
	}
	idx := int(q[0] - '1')

	// 需要至少 3 条历史才能回溯到"上一轮问题 + 引导 + 本轮数字"
	if memCtx == nil || len(memCtx.History) < 3 {
		return false
	}
	h := memCtx.History
	priorUser := h[len(h)-3]
	priorAsst := h[len(h)-2]
	currUser := h[len(h)-1]
	if !strings.EqualFold(priorUser.Role, "user") || !strings.EqualFold(priorAsst.Role, "assistant") || !strings.EqualFold(currUser.Role, "user") {
		return false
	}
	// 只要 assistant 里包含引导前缀，才认定为"引导回复场景"
	if !strings.Contains(priorAsst.Content, guidance.AmbiguityPromptPrefix) {
		return false
	}
	if strings.TrimSpace(currUser.Content) != q {
		return false
	}

	// 取出本会话上轮保存的候选节点
	nodeIDs := s.guidancePending.Peek(req.ConversationID)
	if len(nodeIDs) == 0 || idx < 0 || idx >= len(nodeIDs) {
		return false
	}

	// 用 priorUser（真正的问题）再跑一次 intent，再用选中节点覆盖 Scores，保证强命中
	ig, err := s.intentResolver.Resolve(ctx, []string{priorUser.Content}, roots)
	if err != nil {
		slog.Warn("guidance follow-up intent resolve failed", "err", err)
	}
	if ig == nil {
		ig = &intent.IntentGroup{}
	}
	if len(ig.SubQuestions) == 0 {
		ig.SubQuestions = []intent.SubQuestionIntent{{Question: priorUser.Content}}
	}
	chosen := nodeIDs[idx]
	ig.SubQuestions[0].Scores = []intent.NodeScore{
		{NodeID: chosen, Score: 0.95, Confidence: "HIGH"},
	}
	*intentGroup = ig
	// 清空 pending，避免下一轮继续触发引导逻辑
	s.guidancePending.Clear(req.ConversationID)
	return true
}

// appendMemory 薄封装：memorySvc 未注入时返回空串 + nil，不报错。
func (s *RAGChatService) appendMemory(ctx context.Context, conversationID string, msg chat.Message) (string, error) {
	if s.memorySvc == nil {
		return "", nil
	}
	return s.memorySvc.Append(ctx, conversationID, msg)
}

// executeMCPTool 执行单个 MCP 工具调用。
// 失败路径统一构造 MCPResponse 而不是返回 error：上层要 trace 每次调用的成功率、成本等细节。
func (s *RAGChatService) executeMCPTool(ctx context.Context, userID string, call mcpCallSpec) *mcp.MCPResponse {
	// 1) 找执行器：未注册工具返回 TOOL_NOT_FOUND
	executor, err := s.mcpRegistry.GetExecutor(call.ToolID)
	if err != nil {
		return &mcp.MCPResponse{
			Success:   false,
			ToolID:    call.ToolID,
			ErrorCode: "TOOL_NOT_FOUND",
			ErrorMsg:  fmt.Sprintf("工具不存在: %s", call.ToolID),
		}
	}
	// 2) 抽参：让 LLM 按工具定义 + 子问文本生成参数 JSON
	tool := executor.GetToolDefinition()
	params, _ := s.mcpExtractor.Extract(ctx, tool, call.Question, call.ParamPromptTemplate)
	// 3) 执行
	resp, err := executor.Execute(ctx, mcp.MCPRequest{
		ToolID:     call.ToolID,
		UserID:     userID,
		Parameters: params,
	})
	if err != nil {
		slog.Warn("MCP tool execution failed", "tool", call.ToolID, "err", err)
		return &mcp.MCPResponse{
			Success:   false,
			ToolID:    call.ToolID,
			ErrorCode: "EXECUTION_ERROR",
			ErrorMsg:  err.Error(),
		}
	}
	if resp == nil {
		return &mcp.MCPResponse{
			Success:   false,
			ToolID:    call.ToolID,
			ErrorCode: "NIL_RESPONSE",
			ErrorMsg:  "工具返回空结果",
		}
	}
	return resp
}

// runMCPToolsParallel 与 Java RetrievalEngine#executeMcpTools 对齐，
// 用 errgroup 并发运行 N 个工具调用，结果按调用顺序回填。
// errgroup 在此永不返回错误（executeMCPTool 不向外抛 err），所以 Wait 的 err 忽略。
func (s *RAGChatService) runMCPToolsParallel(ctx context.Context, userID string, calls []mcpCallSpec) []*mcp.MCPResponse {
	out := make([]*mcp.MCPResponse, len(calls))
	g, gCtx := errgroup.WithContext(ctx)
	for i, call := range calls {
		i, call := i, call
		g.Go(func() error {
			out[i] = s.executeMCPTool(gCtx, userID, call)
			return nil
		})
	}
	_ = g.Wait()
	return out
}
