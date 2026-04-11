package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/viper"
)

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

// IsEnabled returns true if Enabled is nil (default) or true.
func (c *ModelCandidate) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// Load reads config.yaml and returns AppConfig.
func Load(path string) (*AppConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// Support env var overrides like ${BAILIAN_API_KEY:default}
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg AppConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Resolve env placeholders in API keys
	for name, p := range cfg.AI.Providers {
		p.APIKey = resolveEnvPlaceholder(p.APIKey)
		cfg.AI.Providers[name] = p
	}
	slog.Info("config loaded", "path", path)
	return &cfg, nil
}

// resolveEnvPlaceholder handles ${ENV_VAR:default} syntax.
func resolveEnvPlaceholder(val string) string {
	if !strings.HasPrefix(val, "${") || !strings.HasSuffix(val, "}") {
		return val
	}
	inner := val[2 : len(val)-1]
	parts := strings.SplitN(inner, ":", 2)
	envVal := os.Getenv(parts[0])
	if envVal == "" {
		return envVal
	}
	if len(parts) > 1 {
		return parts[1]
	}
	return ""
}
