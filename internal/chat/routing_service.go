package chat

import (
	"context"
	"errors"
	"fmt"
	"gogent/internal/config"
	"gogent/internal/model"
	"gogent/pkg/errcode"
	"time"
)

// RoutingLLMService 是带路由功能的 LLM 服务
// 核心职责：
// 1. 多模型候选：配置多个模型（如 qwen-plus → glm-4 → 本地模型）
// 2. 自动降级：当主模型失败时，自动尝试备用模型
// 3. 健康检查：记录模型成功/失败次数，实现熔断机制
// 4. 首包探测：流式响应先等待第一个数据包，确认模型正常后再返回
type RoutingLLMService struct {
	chatExecutor   *model.RoutingExecutor[*ChatResponse]      // 非流式请求的路由执行器
	streamExecutor *model.RoutingExecutor[<-chan StreamDelta] // 流式请求的路由执行器
	aiCfg          config.AIConfig                            // AI 配置（包含所有模型提供商信息）
	openAI         *openAIClient                              // OpenAI 兼容 API 客户端（百炼、硅基流动）
	ollama         *ollamaClient                              // Ollama 本地模型客户端
	selector       *model.Selector                            // 模型选择器（根据是否深度思考选择候选列表）
	health         *model.HealthStore                         // 健康状态存储（记录模型成功/失败次数）
}

// NewRoutingLLMService 创建带路由功能的 LLM 服务实例
// 参数说明：
//   - aiCfg: AI 配置，包含所有模型提供商的 URL、API Key 等
//   - health: 健康状态存储，用于记录模型的成功/失败次数
//   - selector: 模型选择器，根据配置选择合适的候选模型
func NewRoutingLLMService(aiCfg config.AIConfig, health *model.HealthStore, selector *model.Selector) *RoutingLLMService {
	return &RoutingLLMService{
		// 创建两个路由执行器：一个用于非流式，一个用于流式
		// RoutingExecutor 负责：按优先级尝试候选模型、记录健康状态、自动降级
		chatExecutor:   model.NewRoutingExecutor[*ChatResponse](selector, health, aiCfg.Providers),
		streamExecutor: model.NewRoutingExecutor[<-chan StreamDelta](selector, health, aiCfg.Providers),
		aiCfg:          aiCfg,
		openAI:         newOpenAIClient(), // 初始化 OpenAI 兼容客户端（120s 超时）
		ollama:         newOllamaClient(), // 初始化 Ollama 客户端（120s 超时）
		selector:       selector,
		health:         health,
	}
}

// Chat 执行非流式聊天请求（带自动路由和降级）
// 工作流程：
// 1. 构建请求：应用所有 ChatOption（temperature、max_tokens 等）
// 2. 选择候选：根据是否开启深度思考选择不同的模型列表
// 3. 指定模型：如果用户指定了 modelID，则只使用该模型
// 4. 路由执行：按优先级尝试候选模型，失败自动降级
// 5. 返回结果：成功返回响应，所有候选失败返回错误
func (s *RoutingLLMService) Chat(ctx context.Context, messages []Message, opts ...ChatOption) (*ChatResponse, error) {
	// 步骤1：构建基础请求
	req := ChatRequest{Messages: messages, Stream: false}
	for _, opt := range opts {
		opt(&req) // 应用所有选项（如 WithTemperature、WithThinking 等）
	}

	// 步骤2：获取候选模型列表
	// 例如：thinking=false → [qwen-plus, glm-4, qwen-local]
	//      thinking=true  → [qwen3-max, glm-4.7]
	preferredID, candidates := s.chatCandidates(req.Thinking)

	// 步骤3：如果用户指定了模型，则过滤候选列表
	if req.ModelID != "" {
		preferredID = req.ModelID
		candidates = filterCandidatesByID(candidates, req.ModelID)
	}

	// 步骤4：将首选模型放到候选列表第一位
	// 例如：原列表 [A, B, C]，首选 B → [B, A, C]
	candidates = preferFirst(preferredID, candidates)

	// 步骤5：执行路由逻辑
	// ExecuteWithCandidates 会：
	//   a. 按顺序尝试每个候选模型
	//   b. 记录成功/失败次数到 health store
	//   c. 某个模型连续失败达到阈值时触发熔断
	//   d. 所有候选都失败时返回最后的错误
	resp, err := s.chatExecutor.ExecuteWithCandidates(ctx, s.aiCfg.Chat, "chat", candidates, func(ctx context.Context, target model.ModelTarget) (*ChatResponse, error) {
		return s.callByProvider(ctx, target, req) // 调用具体提供商的 API
	})

	// 步骤6：处理错误
	if err != nil {
		return nil, errcode.NewRemoteError("所有模型候选均不可用", err)
	}
	return resp, nil
}

// ChatStream 执行流式聊天请求（带首包探测和自动降级）
// 与 Chat 的区别：
// 1. 流式响应需要首包探测：确认模型真正开始输出后才返回 channel
// 2. 手动实现降级逻辑（而非使用 RoutingExecutor）
// 3. 需要精细的 context 和 channel 管理
//
// 为什么需要首包探测？
// 问题场景：某模型 HTTP 200 成功，但实际生成时报错
// 如果不探测：错误数据会污染前端输出
// 解决方案：等待第一个数据包，确认正常后再返回
func (s *RoutingLLMService) ChatStream(ctx context.Context, messages []Message, opts ...ChatOption) (<-chan StreamDelta, error) {
	// 步骤1：构建流式请求
	req := ChatRequest{Messages: messages, Stream: true}
	for _, opt := range opts {
		opt(&req)
	}

	// 步骤2：获取候选模型列表
	preferredID, candidates := s.chatCandidates(req.Thinking)

	// 步骤3：如果指定了模型，过滤候选列表
	if req.ModelID != "" {
		preferredID = req.ModelID
		candidates = filterCandidatesByID(candidates, req.ModelID)
	}

	// 步骤4：将首选模型放第一位
	candidates = preferFirst(preferredID, candidates)
	if len(candidates) == 0 {
		return nil, errcode.NewRemoteError("无可用的模型候选", nil)
	}

	// 步骤5：首包探测超时时间（60秒）
	// 如果 60 秒内没有收到第一个数据包，认为该模型不可用
	const firstPacketTimeout = 60 * time.Second

	// 步骤6：遍历候选模型，逐个尝试
	var lastErr error
	for _, c := range candidates {
		// 为每个候选创建独立的 context，失败时可以取消
		attemptCtx, cancel := context.WithCancel(ctx)

		// 解析模型目标（获取 URL、API Key 等）
		target, ok := s.resolveTarget(c, "chat")
		if !ok {
			cancel() // 配置不存在，跳过
			continue
		}

		// 调用模型提供商的流式 API
		srcCh, err := s.streamByProvider(attemptCtx, target, req)
		if err != nil {
			cancel()
			lastErr = err
			s.health.RecordFailure(c.ID) // 记录失败
			continue                     // 尝试下一个候选
		}

		// 首包探测：等待第一个有效数据包
		// probeAndBridgeStream 会：
		//   a. 从 srcCh 读取数据，直到收到第一个有内容的 chunk
		//   b. 如果超时或流结束，返回错误
		//   c. 成功后，将 buffered 的数据 + 后续数据桥接到 outCh
		outCh, probeErr := probeAndBridgeStream(attemptCtx, srcCh, firstPacketTimeout)
		if probeErr != nil {
			cancel()
			lastErr = probeErr
			s.health.RecordFailure(c.ID) // 记录失败
			continue                     // 尝试下一个候选
		}

		// 步骤7：首包探测成功，记录成功并返回
		s.health.RecordSuccess(c.ID)

		// 当下游取消或完成时，清理 attempt context
		go func() {
			<-attemptCtx.Done() // 等待下游取消
			cancel()            // 清理资源
		}()
		return outCh, nil // 返回桥接后的 channel
	}

	// 步骤8：所有候选都失败，根据错误类型返回不同的错误码
	if lastErr != nil {
		msg := lastErr.Error()
		switch {
		case errors.Is(lastErr, context.DeadlineExceeded) ||
			msg == "stream first packet timeout":
			// 超时错误：模型响应太慢
			return nil, errcode.NewServiceErrorWith(errcode.ServiceTimeoutError, "模型响应超时", lastErr)
		case msg == "stream ended before first content":
			// 空响应错误：模型连接成功但无输出
			return nil, errcode.NewRemoteError("模型无有效输出", lastErr)
		default:
			// 其他错误：网络错误、API 错误等
			return nil, errcode.NewRemoteError("所有模型候选均不可用", lastErr)
		}
	}
	return nil, errcode.NewRemoteError("所有模型候选均不可用", nil)
}

// ChatWithModelID 使用指定模型执行非流式聊天
// 这是 Chat 的便捷方法，自动添加 modelID 选项
func (s *RoutingLLMService) ChatWithModelID(ctx context.Context, modelID string, messages []Message, opts ...ChatOption) (*ChatResponse, error) {
	opts = append(opts, WithModelID(modelID))
	return s.Chat(ctx, messages, opts...)
}

// ChatStreamWithModelID 使用指定模型执行流式聊天
// 这是 ChatStream 的便捷方法，自动添加 modelID 选项
func (s *RoutingLLMService) ChatStreamWithModelID(ctx context.Context, modelID string, messages []Message, opts ...ChatOption) (<-chan StreamDelta, error) {
	opts = append(opts, WithModelID(modelID))
	return s.ChatStream(ctx, messages, opts...)
}

// callByProvider 根据提供商类型调用对应的客户端
// 支持两种提供商：
//   - ollama: 使用 Ollama 本地模型 API
//   - 其他: 使用 OpenAI 兼容 API（百炼、硅基流动等）
func (s *RoutingLLMService) callByProvider(ctx context.Context, target model.ModelTarget, req ChatRequest) (*ChatResponse, error) {
	switch target.Provider {
	case "ollama":
		return s.ollama.call(ctx, target, req)
	default:
		return s.openAI.call(ctx, target, req)
	}
}

// streamByProvider 根据提供商类型调用对应的流式客户端
// 与 callByProvider 类似，但用于流式响应
func (s *RoutingLLMService) streamByProvider(ctx context.Context, target model.ModelTarget, req ChatRequest) (<-chan StreamDelta, error) {
	switch target.Provider {
	case "ollama":
		return s.ollama.callStream(ctx, target, req)
	default:
		return s.openAI.callStream(ctx, target, req)
	}
}

// chatCandidates 根据是否开启深度思考选择合适的候选模型列表
// 配置示例（config.yaml）：
//
//	ai.chat.candidates:
//	  - id: qwen-plus       (priority: 1, 不支持 thinking)
//	  - id: qwen3-max       (priority: 3, supports_thinking: true)
//	  - id: glm-4.7         (priority: 0, supports_thinking: true)
//
// thinking=false 时：返回 [glm-4.7, qwen-plus, qwen3-max] 按 priority 升序
// thinking=true 时：  返回 [glm-4.7, qwen3-max] 仅包含支持 thinking 的模型
func (s *RoutingLLMService) chatCandidates(thinking bool) (preferredID string, candidates []config.ModelCandidate) {
	return s.selector.SelectChatCandidates(s.aiCfg.Chat, thinking)
}

// resolveTarget 解析模型候选为具体的调用目标
// 将配置信息合并：
//   - candidate: 模型 ID、名称、优先级、是否支持 thinking
//   - provider:  提供商 URL、API Key、端点路径
//
// 返回 ModelTarget 包含：
//   - ID: "qwen-plus"
//   - Provider: "bailian"
//   - Model: "qwen-plus-latest"
//   - URL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"
//   - APIKey: "sk-xxx"
func (s *RoutingLLMService) resolveTarget(c config.ModelCandidate, endpointKey string) (model.ModelTarget, bool) {
	// 查找提供商配置
	provider, ok := s.aiCfg.Providers[c.Provider]
	if !ok {
		return model.ModelTarget{}, false
	}
	// 合并配置，生成完整的调用目标
	return model.ResolveTarget(c, provider, endpointKey), true
}

// preferFirst 将首选模型移到候选列表第一位
// 作用：确保用户指定的模型优先尝试
//
// 示例：
//
//	原列表: [A, B, C, D]
//	首选: C
//	结果: [C, A, B, D]
//
// 注意：保持其他元素的相对顺序不变
func preferFirst(preferredID string, candidates []config.ModelCandidate) []config.ModelCandidate {
	if preferredID == "" || len(candidates) <= 1 {
		return candidates // 无需调整
	}

	var preferred *config.ModelCandidate
	var rest []config.ModelCandidate

	// 遍历候选列表，分离首选和其他
	for _, c := range candidates {
		if c.ID == preferredID && preferred == nil {
			tmp := c
			preferred = &tmp // 找到首选
			continue
		}
		rest = append(rest, c) // 其他候选
	}

	// 如果没找到首选，返回原列表
	if preferred == nil {
		return candidates
	}

	// 首选放第一位，其他保持原顺序
	return append([]config.ModelCandidate{*preferred}, rest...)
}

// filterCandidatesByID 根据模型 ID 过滤候选列表
// 用于用户指定了特定模型时，只保留该模型
//
// 示例：
//
//	原列表: [qwen-plus, glm-4, qwen-local]
//	modelID: "glm-4"
//	结果: [glm-4]
func filterCandidatesByID(candidates []config.ModelCandidate, modelID string) []config.ModelCandidate {
	if modelID == "" {
		return candidates // 不过滤
	}

	filtered := make([]config.ModelCandidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.ID == modelID {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

// probeAndBridgeStream 首包探测 + 流桥接函数
//
// 核心问题：
// 流式响应中，HTTP 200 不代表模型正常工作
// 场景：模型 API 返回 200，但实际生成时报错或返回空
// 如果不探测：错误数据会污染前端，用户看到乱码或空回答
//
// 解决方案：
// 1. 等待第一个有效数据包（Content != "" 或 IsThinking == true）
// 2. 缓冲期间收到的所有数据
// 3. 成功后，将缓冲数据 + 后续数据桥接到新的 channel
// 4. 超时或失败，返回错误让调用者尝试下一个候选
//
// 参数：
//   - ctx: 上下文（用于取消控制）
//   - src: 原始流 channel（从模型 API 读取）
//   - timeout: 首包超时时间（60秒）
//
// 返回：
//   - outCh: 桥接后的 channel（下游消费）
//   - error: 探测失败原因（超时、空流、API 错误等）
func probeAndBridgeStream(ctx context.Context, src <-chan StreamDelta, timeout time.Duration) (<-chan StreamDelta, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// 缓冲首包探测期间收到的数据
	var buffered []StreamDelta

	// 阶段1：首包探测
	// 等待第一个有效数据包，或超时/失败
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // 上下文取消
		case <-timer.C:
			return nil, fmt.Errorf("stream first packet timeout") // 超时
		case d, ok := <-src:
			if !ok {
				return nil, fmt.Errorf("stream ended before first content") // 流结束但无内容
			}
			if d.Err != nil {
				return nil, d.Err // API 错误
			}

			// 收到数据，缓冲起来
			buffered = append(buffered, d)

			// 检查是否是有效数据包
			if d.Content != "" || d.IsThinking {
				// 找到首包！进入阶段2
				out := make(chan StreamDelta, 64)

				// 阶段2：桥接流
				// Goroutine 负责：
				//   a. 先发送缓冲的数据
				//   b. 再转发后续的 src 数据
				go func() {
					defer close(out)

					// 步骤1：发送缓冲的数据
					for _, b := range buffered {
						select {
						case <-ctx.Done():
							return
						case out <- b:
						}
					}

					// 步骤2：转发后续数据
					for dd := range src {
						select {
						case <-ctx.Done():
							return
						case out <- dd:
						}
						if dd.Err != nil {
							return // 遇到错误，停止
						}
					}
				}()

				return out, nil // 返回桥接后的 channel
			}
		}
	}
}
