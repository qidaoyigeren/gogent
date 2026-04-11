package main

// AppConfig is the root configuration struct.
type AppConfig struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	Redis    RedisConfig    `mapstructure:"redis"`
	Milvus   MilvusConfig   `mapstructure:"milvus"`
	RAG      RAGConfig      `mapstructure:"rag"`
	AI       AIConfig       `mapstructure:"ai"`
	Storage  StorageConfig  `mapstructure:"storage"`
}

type ServerConfig struct {
	Port        int    `mapstructure:"port"`
	ContextPath string `mapstructure:"context-path"`
}

type DatabaseConfig struct {
	Driver          string `mapstructure:"driver"`
	DSN             string `mapstructure:"dsn"`
	MaxOpenConns    int    `mapstructure:"max-open-conns"`
	MaxIdleConns    int    `mapstructure:"max-idle-conns"`
	ConnMaxLifetime string `mapstructure:"conn-max-lifetime"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type MilvusConfig struct {
	URI string `mapstructure:"uri"`
}

type StorageConfig struct {
	BasePath string `mapstructure:"base-path"`
}

type RAGConfig struct {
	Vector       RAGVectorConfig    `mapstructure:"vector"`
	Default      RAGDefaultConfig   `mapstructure:"default"`
	QueryRewrite QueryRewriteConfig `mapstructure:"query-rewrite"`
	RateLimit    RateLimitConfig    `mapstructure:"rate-limit"`
	Memory       MemoryConfig       `mapstructure:"memory"`
	MCP          MCPConfig          `mapstructure:"mcp"`
	Search       SearchConfig       `mapstructure:"search"`
}

type RAGVectorConfig struct {
	Type string `mapstructure:"type"`
}

type RAGDefaultConfig struct {
	CollectionName string `mapstructure:"collection-name"`
	Dimension      int    `mapstructure:"dimension"`
	MetricType     string `mapstructure:"metric-type"`
}

type QueryRewriteConfig struct {
	Enabled            bool `mapstructure:"enabled"`
	MaxHistoryMessages int  `mapstructure:"max-history-messages"`
	MaxHistoryChars    int  `mapstructure:"max-history-chars"`
}

type RateLimitConfig struct {
	Global RateLimitGlobalConfig `mapstructure:"global"`
}

type RateLimitGlobalConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	MaxConcurrent  int  `mapstructure:"max-concurrent"`
	MaxWaitSeconds int  `mapstructure:"max-wait-seconds"`
	LeaseSeconds   int  `mapstructure:"lease-seconds"`
	PollIntervalMs int  `mapstructure:"poll-interval-ms"`
}

type MemoryConfig struct {
	HistoryKeepTurns  int  `mapstructure:"history-keep-turns"`
	SummaryStartTurns int  `mapstructure:"summary-start-turns"`
	SummaryEnabled    bool `mapstructure:"summary-enabled"`
	TTLMinutes        int  `mapstructure:"ttl-minutes"`
	SummaryMaxChars   int  `mapstructure:"summary-max-chars"`
	TitleMaxLength    int  `mapstructure:"title-max-length"`
}

type MCPConfig struct {
	Servers []MCPServerConfig `mapstructure:"servers"`
}

type MCPServerConfig struct {
	Name string `mapstructure:"name"`
	URL  string `mapstructure:"url"`
}

type SearchConfig struct {
	Channels SearchChannelsConfig `mapstructure:"channels"`
}

type SearchChannelsConfig struct {
	VectorGlobal   ChannelConfig `mapstructure:"vector-global"`
	IntentDirected ChannelConfig `mapstructure:"intent-directed"`
}

type ChannelConfig struct {
	ConfidenceThreshold float64 `mapstructure:"confidence-threshold"`
	TopKMultiplier      int     `mapstructure:"top-k-multiplier"`
	MinIntentScore      float64 `mapstructure:"min-intent-score"`
}

// AIConfig mirrors Java AIModelProperties.
type AIConfig struct {
	Providers map[string]ProviderConfig `mapstructure:"providers"`
	Selection SelectionConfig           `mapstructure:"selection"`
	Stream    StreamConfig              `mapstructure:"stream"`
	Chat      ModelGroupConfig          `mapstructure:"chat"`
	Embedding ModelGroupConfig          `mapstructure:"embedding"`
	Rerank    ModelGroupConfig          `mapstructure:"rerank"`
}

type ProviderConfig struct {
	URL       string            `mapstructure:"url"`
	APIKey    string            `mapstructure:"api-key"`
	Endpoints map[string]string `mapstructure:"endpoints"`
}

type SelectionConfig struct {
	FailureThreshold int   `mapstructure:"failure-threshold"`
	OpenDurationMs   int64 `mapstructure:"open-duration-ms"`
}

type StreamConfig struct {
	MessageChunkSize int `mapstructure:"message-chunk-size"`
}

type ModelGroupConfig struct {
	DefaultModel      string           `mapstructure:"default-model"`
	DeepThinkingModel string           `mapstructure:"deep-thinking-model"`
	Candidates        []ModelCandidate `mapstructure:"candidates"`
}

type ModelCandidate struct {
	ID               string `mapstructure:"id"`
	Provider         string `mapstructure:"provider"`
	Model            string `mapstructure:"model"`
	URL              string `mapstructure:"url"`
	Dimension        int    `mapstructure:"dimension"`
	Priority         int    `mapstructure:"priority"`
	Enabled          *bool  `mapstructure:"enabled"`
	SupportsThinking bool   `mapstructure:"supports-thinking"`
}
