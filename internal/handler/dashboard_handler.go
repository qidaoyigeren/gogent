package handler

import (
	"fmt"
	"gogent/internal/entity"
	"gogent/pkg/response"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// DashboardHandler handles admin dashboard endpoints.
type DashboardHandler struct{ db *gorm.DB }

func NewDashboardHandler(db *gorm.DB) *DashboardHandler {
	return &DashboardHandler{db: db}
}

// RegisterRoutes 注册管理端概览、性能和趋势图接口。
func (h *DashboardHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/dashboard/overview", h.overview)
	rg.GET("/admin/dashboard/performance", h.performance)
	rg.GET("/admin/dashboard/trends", h.trends)
}

// --- Window parsing ---

type windowRange struct {
	start, end         time.Time
	prevStart, prevEnd time.Time
	label              string
}

func parseWindowRange(window string) windowRange {
	now := time.Now()
	dur := 24 * time.Hour
	if strings.HasSuffix(window, "d") {
		var d int
		fmt.Sscanf(window, "%dd", &d)
		if d > 0 {
			dur = time.Duration(d) * 24 * time.Hour
		}
	} else if strings.HasSuffix(window, "h") {
		var h int
		fmt.Sscanf(window, "%dh", &h)
		if h > 0 {
			dur = time.Duration(h) * time.Hour
		}
	}
	end := now
	start := now.Add(-dur)
	// 统一使用 [start, end) 半开区间，避免相邻窗口统计重复计数边界点。
	// prevStart/prevEnd 用于和当前窗口等长的上一周期做环比。
	prevEnd := start
	prevStart := start.Add(-dur)
	return windowRange{start: start, end: end, prevStart: prevStart, prevEnd: prevEnd, label: window}
}

//  1. /admin/dashboard/overview（KPI 概览）
//     返回用户总数、活跃用户、会话数、消息数及窗口内增量/环比百分比。
//     parseWindowRange 解析 "24h" / "7d" 等时间窗口，同时计算上一等长周期供环比。
//
// overview 返回管理端 KPI：用户、活跃用户、会话和消息的总量/窗口增量。
func (h *DashboardHandler) overview(c *gin.Context) {
	window := c.DefaultQuery("window", "24h")
	wr := parseWindowRange(window)

	// Total counts
	var totalUsers, totalSessions, totalMessages int64
	h.db.Model(&entity.UserDO{}).Count(&totalUsers)
	h.db.Model(&entity.ConversationDO{}).Count(&totalSessions)
	h.db.Model(&entity.ConversationMessageDO{}).Count(&totalMessages)

	// Window counts
	var sessionsInWindow, messageInWindow int64
	h.db.Model(&entity.ConversationDO{}).Where("create_time >= ? AND create_time < ?", wr.start, wr.end).Count(&sessionsInWindow)
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ?", wr.start, wr.end).Count(&messageInWindow)

	// Previous window counts (for delta)
	var sessionsPrev, messagesPrev int64
	h.db.Model(&entity.ConversationDO{}).Where("create_time >= ? AND create_time < ?", wr.prevStart, wr.prevEnd).Count(&sessionsPrev)
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ?", wr.prevStart, wr.prevEnd).Count(&messagesPrev)

	// Active users (distinct user_id in messages within window)
	// 活跃定义来自消息表而非会话表，体现“真实发言用户”而不是“创建了会话但无消息”用户。
	var activeUsers int64
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ?", wr.start, wr.end).
		Distinct("user_id").Count(&activeUsers)
	var activeUsersPrev int64
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ?", wr.prevStart, wr.prevEnd).
		Distinct("user_id").Count(&activeUsersPrev)

	response.Success(c, gin.H{
		"window":        window,
		"compareWindow": "prev",
		"updatedAt":     time.Now().UnixMilli(),
		"kpis": gin.H{
			"totalUsers":    buildKpi(totalUsers, 0, nil),
			"activeUsers":   buildKpi(activeUsers, activeUsers-activeUsersPrev, calcPct(activeUsers, activeUsersPrev)),
			"totalSessions": buildKpi(totalSessions, sessionsInWindow, nil),
			"sessions24h":   buildKpi(sessionsInWindow, sessionsInWindow-sessionsPrev, calcPct(sessionsInWindow, sessionsPrev)),
			"totalMessages": buildKpi(totalMessages, messageInWindow, nil),
			"messages24h":   buildKpi(messageInWindow, messageInWindow-messagesPrev, calcPct(messageInWindow, messagesPrev)),
		},
	})
}

// buildKpi 统一封装 KPI 值、绝对增量和可选百分比增量。
func buildKpi(value, delta int64, deltaPct *float64) gin.H {
	kpi := gin.H{"value": value, "delta": delta}
	if deltaPct != nil {
		kpi["deltaPct"] = *deltaPct
	}
	return kpi
}

// calcPct 计算环比百分比；上一窗口为 0 时返回 nil，避免除零和误导性百分比。
func calcPct(current, previous int64) *float64 {
	if previous == 0 {
		return nil
	}
	pct := float64(current-previous) / float64(previous) * 100
	pct = math.Round(pct*10) / 10
	return &pct
}

//  2. /admin/dashboard/performance（RAG 性能指标）
//     从 rag_trace_run 表读取 duration_ms，计算平均延迟、P95 延迟、成功率、错误率、
//     无知识率（"未检索到"回复占比）和慢请求率（> 3000ms）。
//
// performance 统计 RAG 链路性能指标：平均延迟、P95、成功率、错误率、无知识率和慢请求率。
func (h *DashboardHandler) performance(c *gin.Context) {
	window := c.DefaultQuery("window", "24h")
	wr := parseWindowRange(window)

	// Get all trace run durations in the window
	var durations []int64
	h.db.Model(&entity.RagTraceRunDO{}).Where("create_time >= ? AND create_time < ?", wr.start, wr.end).
		Pluck("duration_ms", &durations)

	avgLatency := average(durations)
	p95Latency := percentile95(durations)
	// average/p95 都基于 rag_trace_run，避免混入聊天消息表导致口径漂移。

	// Success/error counts
	var successCount, errorCount int64
	h.db.Model(&entity.RagTraceRunDO{}).Where("create_time >= ? AND create_time < ? AND status = ?", wr.start, wr.end, "SUCCESS").Count(&successCount)
	h.db.Model(&entity.RagTraceRunDO{}).Where("create_time >= ? AND create_time < ? AND status = ?", wr.start, wr.end, "ERROR").Count(&errorCount)
	total := successCount + errorCount

	// Slow requests: duration > 3000 ms among all runs in the window.
	const slowThresholdMs = 3000
	var slowCount int64
	h.db.Model(&entity.RagTraceRunDO{}).
		Where("create_time >= ? AND create_time < ? AND duration_ms > ?", wr.start, wr.end, slowThresholdMs).
		Count(&slowCount)

	// Assistant messages and "no doc" messages
	var assistantCount int64
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ? AND role = ?", wr.start, wr.end, "assistant").Count(&assistantCount)
	var noDocCount int64
	h.db.Model(&entity.ConversationMessageDO{}).Where("create_time >= ? AND create_time < ? AND role = ? AND content LIKE ?",
		wr.start, wr.end, "assistant", "%未检索到%").Count(&noDocCount)

	var successRate, errorRate, noDocRate, slowRate float64
	if total > 0 {
		successRate = math.Round(float64(successCount)/float64(total)*1000) / 10
		errorRate = math.Round(float64(errorCount)/float64(total)*1000) / 10
		slowRate = math.Round(float64(slowCount)/float64(total)*1000) / 10
	}
	if assistantCount > 0 {
		// noDocRate 的分母是 assistant 消息数，不是 trace run 数，代表“用户可见回答质量”视角。
		noDocRate = math.Round(float64(noDocCount)/float64(assistantCount)*1000) / 10
	}

	response.Success(c, gin.H{
		"window":       window,
		"avgLatencyMs": avgLatency,
		"p95LatencyMs": p95Latency,
		"successRate":  successRate,
		"errorRate":    errorRate,
		"noDocRate":    noDocRate,
		"slowRate":     slowRate,
	})
}

// average 计算 int64 平均值，空切片返回 0。
func average(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	var sum int64
	for _, v := range vals {
		sum += v
	}
	return sum / int64(len(vals))
}

// percentile95 计算 P95 延迟，先复制排序，避免修改原始 durations。
func percentile95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]int64, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	// 简化分位实现：离散索引截取，不做插值；运维看趋势足够稳定。
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// --- Trends ---

type trendPoint struct {
	Ts    int64   `json:"ts"`
	Value float64 `json:"value"`
}

type trendSeries struct {
	Name string       `json:"name"`
	Data []trendPoint `json:"data"`
}

//  3. /admin/dashboard/trends（时间序列趋势图）
//     支持 sessions / messages / activeusers / avglatency / quality 五类指标。
//     按 granularity（hour/day）分桶聚合，fillTimeSlots 补齐空桶保证图表横轴连续。
func (h *DashboardHandler) trends(c *gin.Context) {
	metric := c.DefaultQuery("metric", "sessions")
	window := c.DefaultQuery("window", "7d")
	granularity := c.DefaultQuery("granularity", "")

	wr := parseWindowRange(window)

	// Auto-resolve granularity
	// 短窗口默认按小时，长窗口默认按天，减少前端传参负担。
	if granularity == "" {
		if wr.end.Sub(wr.start) <= 48*time.Hour {
			granularity = "hour"
		} else {
			granularity = "day"
		}
	}

	var format string
	var step time.Duration
	if granularity == "hour" {
		format = "2006-01-02 15:00:00"
		step = time.Hour
	} else {
		format = "2006-01-02"
		step = 24 * time.Hour
	}

	normalizedMetric := strings.ToLower(strings.TrimSpace(metric))
	var series []trendSeries

	switch normalizedMetric {
	case "sessions":
		points := h.countByTime(&entity.ConversationDO{}, "create_time", wr, format, step, "")
		series = []trendSeries{{Name: "会话数", Data: points}}
	case "messages":
		points := h.countByTime(&entity.ConversationMessageDO{}, "create_time", wr, format, step, "")
		series = []trendSeries{{Name: "消息数", Data: points}}
	case "activeusers":
		points := h.countDistinctByTime(&entity.ConversationMessageDO{}, "user_id", "create_time", wr, format, step)
		series = []trendSeries{{Name: "活跃用户", Data: points}}
	case "avglatency":
		points := h.avgByTime("start_time", wr, format, step)
		series = []trendSeries{{Name: "平均响应时间", Data: points}}
	case "quality":
		errorPoints, noDocPoints := h.qualityByTime(wr, format, step)
		series = []trendSeries{
			{Name: "错误率", Data: errorPoints},
			{Name: "无知识率", Data: noDocPoints},
		}
	default:
		// 未知指标回退 sessions 计数，但名称沿用原 metric，便于前端发现配置问题。
		points := h.countByTime(&entity.ConversationDO{}, "create_time", wr, format, step, "")
		series = []trendSeries{{Name: metric, Data: points}}
	}

	response.Success(c, gin.H{
		"metric":      metric,
		"window":      window,
		"granularity": granularity,
		"series":      series,
	})
}

// countByTime groups records by time bucket and counts them.
func (h *DashboardHandler) countByTime(model interface{}, timeCol string, wr windowRange, format string, step time.Duration, extraWhere string) []trendPoint {
	type row struct {
		Bucket string
		Cnt    int64
	}

	var pgFormat string
	if step == time.Hour {
		pgFormat = "YYYY-MM-DD HH24:00:00"
	} else {
		pgFormat = "YYYY-MM-DD"
	}

	var rows []row
	q := h.db.Model(model).
		Select(fmt.Sprintf("to_char(%s, '%s') as bucket, count(*) as cnt", timeCol, pgFormat)).
		Where(fmt.Sprintf("%s >= ? AND %s < ?", timeCol, timeCol), wr.start, wr.end).
		Group("bucket").Order("bucket")
	if extraWhere != "" {
		// extraWhere 仅用于少量派生指标，避免为每类指标复制一套 SQL 结构。
		q = q.Where(extraWhere)
	}
	q.Find(&rows)

	// Build lookup
	// 数据库只返回有数据的桶，lookup 用于后续补齐空桶。
	lookup := make(map[string]int64)
	for _, r := range rows {
		lookup[r.Bucket] = r.Cnt
	}

	// Fill all time slots
	return fillTimeSlots(wr.start, wr.end, step, format, lookup)
}

// countDistinctByTime 按时间桶统计去重数量，例如活跃用户数。
func (h *DashboardHandler) countDistinctByTime(model interface{}, distinctCol, timeCol string, wr windowRange, format string, step time.Duration) []trendPoint {
	type row struct {
		Bucket string
		Cnt    int64
	}

	var pgFormat string
	if step == time.Hour {
		pgFormat = "YYYY-MM-DD HH24:00:00"
	} else {
		pgFormat = "YYYY-MM-DD"
	}

	var rows []row
	h.db.Model(model).
		Select(fmt.Sprintf("to_char(%s, '%s') as bucket, count(distinct %s) as cnt", timeCol, pgFormat, distinctCol)).
		Where(fmt.Sprintf("%s >= ? AND %s < ?", timeCol, timeCol), wr.start, wr.end).
		Group("bucket").Order("bucket").
		Find(&rows)

	lookup := make(map[string]int64)
	for _, r := range rows {
		lookup[r.Bucket] = r.Cnt
	}

	return fillTimeSlots(wr.start, wr.end, step, format, lookup)
}

// avgByTime computes average duration_ms per time bucket for successful trace runs.
func (h *DashboardHandler) avgByTime(timeCol string, wr windowRange, format string, step time.Duration) []trendPoint {
	type row struct {
		Bucket string
		Avg    float64
	}

	var pgFormat string
	if step == time.Hour {
		pgFormat = "YYYY-MM-DD HH24:00:00"
	} else {
		pgFormat = "YYYY-MM-DD"
	}

	var rows []row
	h.db.Model(&entity.RagTraceRunDO{}).
		Select(fmt.Sprintf("to_char(%s, '%s') as bucket, avg(duration_ms) as avg", timeCol, pgFormat)).
		Where(fmt.Sprintf("%s >= ? AND %s < ? AND status = 'SUCCESS'", timeCol, timeCol), wr.start, wr.end).
		Group("bucket").Order("bucket").
		Find(&rows)
	// 只统计 SUCCESS 的平均耗时，避免 ERROR 的短路请求把延迟压低导致误判。

	lookup := make(map[string]float64)
	for _, r := range rows {
		lookup[r.Bucket] = math.Round(r.Avg*10) / 10
	}

	return fillTimeSlotsFloat(wr.start, wr.end, step, format, lookup)
}

// qualityByTime computes error rate and no-doc rate per time bucket.
func (h *DashboardHandler) qualityByTime(wr windowRange, format string, step time.Duration) ([]trendPoint, []trendPoint) {
	var pgFormat string
	if step == time.Hour {
		pgFormat = "YYYY-MM-DD HH24:00:00"
	} else {
		pgFormat = "YYYY-MM-DD"
	}

	type row struct {
		Bucket string
		Cnt    int64
	}

	// Success trace runs
	var successRows []row
	h.db.Model(&entity.RagTraceRunDO{}).
		Select(fmt.Sprintf("to_char(start_time, '%s') as bucket, count(*) as cnt", pgFormat)).
		Where("start_time >= ? AND start_time < ? AND status = 'SUCCESS'", wr.start, wr.end).
		Group("bucket").Find(&successRows)
	successMap := make(map[string]int64)
	for _, r := range successRows {
		successMap[r.Bucket] = r.Cnt
	}

	// Error trace runs
	var errorRows []row
	h.db.Model(&entity.RagTraceRunDO{}).
		Select(fmt.Sprintf("to_char(start_time, '%s') as bucket, count(*) as cnt", pgFormat)).
		Where("start_time >= ? AND start_time < ? AND status = 'ERROR'", wr.start, wr.end).
		Group("bucket").Find(&errorRows)
	errorMap := make(map[string]int64)
	for _, r := range errorRows {
		errorMap[r.Bucket] = r.Cnt
	}

	// Assistant messages
	var assistantRows []row
	h.db.Model(&entity.ConversationMessageDO{}).
		Select(fmt.Sprintf("to_char(create_time, '%s') as bucket, count(*) as cnt", pgFormat)).
		Where("create_time >= ? AND create_time < ? AND role = 'assistant'", wr.start, wr.end).
		Group("bucket").Find(&assistantRows)
	assistantMap := make(map[string]int64)
	for _, r := range assistantRows {
		assistantMap[r.Bucket] = r.Cnt
	}

	// No-doc messages
	var noDocRows []row
	h.db.Model(&entity.ConversationMessageDO{}).
		Select(fmt.Sprintf("to_char(create_time, '%s') as bucket, count(*) as cnt", pgFormat)).
		Where("create_time >= ? AND create_time < ? AND role = 'assistant' AND content = '未检索到与问题相关的文档内容。'", wr.start, wr.end).
		Group("bucket").Find(&noDocRows)
	noDocMap := make(map[string]int64)
	for _, r := range noDocRows {
		noDocMap[r.Bucket] = r.Cnt
	}

	// Calculate rates
	errorRateMap := make(map[string]float64)
	noDocRateMap := make(map[string]float64)

	var t time.Time
	if step >= 24*time.Hour {
		t = time.Date(wr.start.Year(), wr.start.Month(), wr.start.Day(), 0, 0, 0, 0, wr.start.Location())
	} else {
		t = wr.start.Truncate(step)
	}

	for t.Before(wr.end) {
		key := t.Format(format)
		total := successMap[key] + errorMap[key]
		if total > 0 {
			// 错误率按 trace run 的成功/失败计算。
			errorRateMap[key] = math.Round(float64(errorMap[key])*1000/float64(total)) / 10
		}
		if assistantMap[key] > 0 {
			// 无知识率按 assistant 消息中“未检索到”回复占比计算。
			noDocRateMap[key] = math.Round(float64(noDocMap[key])*1000/float64(assistantMap[key])) / 10
		}
		t = t.Add(step)
	}

	return fillTimeSlotsFloat(wr.start, wr.end, step, format, errorRateMap),
		fillTimeSlotsFloat(wr.start, wr.end, step, format, noDocRateMap)
}

// fillTimeSlotsFloat generates a continuous time series with float64 lookup.
func fillTimeSlotsFloat(start, end time.Time, step time.Duration, format string, lookup map[string]float64) []trendPoint {
	var points []trendPoint
	var t time.Time
	if step >= 24*time.Hour {
		t = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	} else {
		t = start.Truncate(step)
	}

	for t.Before(end) {
		// 没查到的时间桶填 0，保证图表横轴连续。
		key := t.Format(format)
		val := float64(0)
		if v, ok := lookup[key]; ok {
			val = v
		}
		points = append(points, trendPoint{Ts: t.UnixMilli(), Value: val})
		t = t.Add(step)
	}

	if len(points) == 0 {
		points = []trendPoint{}
	}
	return points
}

// fillTimeSlots generates a continuous time series, filling missing slots with 0.
func fillTimeSlots(start, end time.Time, step time.Duration, format string, lookup map[string]int64) []trendPoint {
	var points []trendPoint
	// Truncate start to step boundary
	var t time.Time
	if step >= 24*time.Hour {
		t = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	} else {
		t = start.Truncate(step)
	}

	for t.Before(end) {
		key := t.Format(format)
		val := float64(0)
		if v, ok := lookup[key]; ok {
			val = float64(v)
		}
		points = append(points, trendPoint{Ts: t.UnixMilli(), Value: val})
		t = t.Add(step)
	}

	if len(points) == 0 {
		points = []trendPoint{}
	}
	return points
}
