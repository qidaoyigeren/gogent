package handler

import (
	"gogent/internal/config"
	"gogent/pkg/response"

	"github.com/gin-gonic/gin"
)

// SettingsHandler 只读 AppConfig；不依赖 DB/Redis，启动就绪即可提供服务。
type SettingsHandler struct {
	cfg *config.AppConfig
}

func NewSettingsHandler(cfg *config.AppConfig) *SettingsHandler {
	return &SettingsHandler{cfg: cfg}
}

func (h *SettingsHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/rag/settings", h.getSettings)
}

// getSettings 聚合运行时配置给前端。
// 注意：API Key 等敏感字段当前也随 providers 返回（但生产需要评估是否脱敏）。
func (h *SettingsHandler) getSettings(c *gin.Context) {
	response.Success(c, gin.H{
		"upload": gin.H{
			"maxFileSize":    50 * 1024 * 1024, // 单文件 50MB
			"maxRequestSize": 104857600,        // 整请求 100MB
		},
		"rag": gin.H{
			// 默认向量集合参数：新建知识库时前端可预填
			"default": gin.H{
				"collectionName": h.cfg.RAG.Default.CollectionName,
				"dimension":      h.cfg.RAG.Default.Dimension,
				"metricType":     h.cfg.RAG.Default.MetricType,
			},
			// 问题重写（rewrite）开关与历史窗口参数
			"queryRewrite": gin.H{
				"enabled":            h.cfg.RAG.QueryRewrite.Enabled,
				"maxHistoryMessages": h.cfg.RAG.QueryRewrite.MaxHistoryMessages,
				"maxHistoryChars":    h.cfg.RAG.QueryRewrite.MaxHistoryChars,
			},
			// 全局限流参数：前端在监控面板展示
			"rateLimit": gin.H{
				"global": gin.H{
					"enabled":        h.cfg.RAG.RateLimit.Global.Enabled,
					"maxConcurrent":  h.cfg.RAG.RateLimit.Global.MaxConcurrent,
					"maxWaitSeconds": h.cfg.RAG.RateLimit.Global.MaxWaitSeconds,
					"leaseSeconds":   h.cfg.RAG.RateLimit.Global.LeaseSeconds,
					"pollIntervalMs": h.cfg.RAG.RateLimit.Global.PollIntervalMs,
				},
			},
			// 记忆/摘要相关配置：决定前端是否显示“开始摘要”提示
			"memory": gin.H{
				"historyKeepTurns":  h.cfg.RAG.Memory.HistoryKeepTurns,
				"ttlMinutes":        h.cfg.RAG.Memory.TTLMinutes,
				"summaryEnabled":    h.cfg.RAG.Memory.SummaryEnabled,
				"summaryStartTurns": h.cfg.RAG.Memory.SummaryStartTurns,
				"summaryMaxChars":   h.cfg.RAG.Memory.SummaryMaxChars,
				"titleMaxLength":    h.cfg.RAG.Memory.TitleMaxLength,
			},
		},
		"ai": gin.H{
			"providers": buildProviderSettings(h.cfg.AI.Providers),
			"chat":      buildModelGroupSettings(h.cfg.AI.Chat),
			"embedding": buildModelGroupSettings(h.cfg.AI.Embedding),
			"rerank":    buildModelGroupSettings(h.cfg.AI.Rerank),
			// 熔断参数：失败次数到阈值则打开熔断 openDurationMs 毫秒
			"selection": gin.H{
				"failureThreshold": h.cfg.AI.Selection.FailureThreshold,
				"openDurationMs":   h.cfg.AI.Selection.OpenDurationMs,
			},
			// 流式回放切片大小：影响 SSE 抖动与前端渲染平滑度
			"stream": gin.H{
				"messageChunkSize": h.cfg.AI.Stream.MessageChunkSize,
			},
		},
	})
}

// buildProviderSettings 把 map[providerName]ProviderConfig 原样转为 gin.H（展开 map）。
// 输出键包括 url / apiKey / endpoints，前端展示供应商路由表用。
func buildProviderSettings(providers map[string]config.ProviderConfig) gin.H {
	out := gin.H{}
	for name, provider := range providers {
		out[name] = gin.H{
			"url":       provider.URL,
			"apiKey":    provider.APIKey,
			"endpoints": provider.Endpoints,
		}
	}
	return out
}

// buildModelGroupSettings 把某一类 modelGroup（chat/embedding/rerank）转成前端友好结构。
// 其中 enabled 调用 IsEnabled() 方法（可能基于 status、过期时间综合判定）。
func buildModelGroupSettings(group config.ModelGroupConfig) gin.H {
	candidates := make([]gin.H, 0, len(group.Candidates))
	for _, candidate := range group.Candidates {
		candidates = append(candidates, gin.H{
			"id":               candidate.ID,
			"provider":         candidate.Provider,
			"model":            candidate.Model,
			"url":              candidate.URL,
			"dimension":        candidate.Dimension,
			"priority":         candidate.Priority,
			"enabled":          candidate.IsEnabled(),
			"supportsThinking": candidate.SupportsThinking,
		})
	}

	return gin.H{
		"defaultModel":      group.DefaultModel,      // 普通请求默认走的模型
		"deepThinkingModel": group.DeepThinkingModel, // deepThinking=true 时走的模型
		"candidates":        candidates,
	}
}
