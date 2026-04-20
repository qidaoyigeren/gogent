package retrieve

import "context"

// DocumentChunk 表示从向量库检索到的文档块
// 这是检索引擎的核心数据结构，在各个通道之间传递
type DocumentChunk struct {
	ID          string            `json:"id"`                 // Chunk ID（唯一标识）
	Content     string            `json:"content"`            // Chunk 内容（文本）
	Score       float64           `json:"score"`              // 相似度分数（越高越相关）
	KBID        string            `json:"kbId"`               // 知识库 ID（所属知识库）
	ChannelName string            `json:"channelName"`        // 通道名称（来源通道，如 "intent-directed"）
	Metadata    map[string]string `json:"metadata,omitempty"` // 元数据（额外信息）
}

// SearchChannel 检索通道接口
// 核心职责：
// 1. 定义统一的检索接口，支持多种检索策略
// 2. 每个通道实现不同的检索逻辑（如意图定向、全局向量）
// 3. 通过优先级和启用状态控制通道的执行顺序
type SearchChannel interface {
	// Name 返回通道名称（用于日志和调试）
	Name() string

	// Priority 返回通道优先级（数字越小优先级越高）
	// 例如：intent-directed=1, vector-global=10
	Priority() int

	// IsEnabled 判断通道是否启用
	IsEnabled(ctx context.Context, reqCtx *RetrievalContext) bool

	// Search 执行检索
	Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error)
}

// RetrievalContext 检索请求上下文
// 携带检索所需的所有参数和状态信息
type RetrievalContext struct {
	Query        string   // 用户查询文本
	SubQuestions []string // 子问题列表（由查询重写生成）
	IntentKBIDs  []string // 意图识别匹配的知识库 ID 列表

	// IntentKBMaxScore 映射每个意图知识库 ID 到其最大分类器分数
	// 用于 Java IntentDirectedSearchChannel 的 minIntentScore 门控
	// 例如：{"kb_123": 0.85, "kb_456": 0.72}
	IntentKBMaxScore map[string]float64

	TopK   int    // 返回结果数量
	UserID string // 用户 ID

	// IntentScoreCount / MaxIntentScore 镜像 Java SearchContext intents
	// 用于 vector-global 通道的启用门控
	IntentScoreCount int     // 意图分类器分数数量
	MaxIntentScore   float64 // 最大意图分数
}

// PostProcessor 后处理器接口
// 核心职责：
// 1. 对通道聚合后的结果进行处理（去重、重排序等）
// 2. 通过 Order() 控制执行顺序
// 3. 支持链式处理（多个 PostProcessor 依次执行）
type PostProcessor interface {
	// Order 返回执行顺序（数字越小越先执行）
	// 例如：dedup=1, rerank=10
	Order() int

	// Process 处理检索结果
	Process(ctx context.Context, query string, chunks []DocumentChunk) ([]DocumentChunk, error)
}
