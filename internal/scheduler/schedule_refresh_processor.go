package scheduler

import (
	"context"
	"gogent/internal/entity"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// schedule_refresh_processor.go 是调度任务的执行核心。
// ScheduleRefreshProcessor.Process 是单次调度刷新的完整流程：
//   1. 启动心跳（StartHeartbeat）延续数据库锁租约，防止长时刷新被判为超时释放
//   2. 加载 schedule 记录和关联文档
//   3. shouldRun 校验：URL 来源 + 文档和调度均启用 + cron 非空 → 才继续
//   4. 创建执行记录（createExec），在审计表留下每次刷新的开始时间
//   5. 检查文档是否已 running，是则标记 skipped 跳过（避免并发刷新）
//   6. 调用 DocumentRefreshRunner.RefreshDocument（即 KnowledgeDocHandler.RefreshDocument）
//   7. 写执行结果（finishExec）和下次运行时间（updateScheduleState）
//
// DocumentRefreshRunner 是接口，使 ScheduleRefreshProcessor 不直接依赖 handler 包，
// 符合依赖倒置原则；实际注入时 main.go 传入 *KnowledgeDocHandler。
//
// DocumentStatusHelper 是辅助工具，供 RecoverStuckRunning 重置卡死文档状态。

type DocumentRefreshRunner interface {
	RefreshDocument(ctx context.Context, docID string) error
}

// ScheduleRefreshProcessor processes scheduled document refreshes.
type ScheduleRefreshProcessor struct {
	db      *gorm.DB
	lockMgr *ScheduleLockManager
	runner  DocumentRefreshRunner
	parser  cron.Parser
}

// NewScheduleRefreshProcessor creates a new refresh processor.
func NewScheduleRefreshProcessor(
	db *gorm.DB,
	lockMgr *ScheduleLockManager,
	runner DocumentRefreshRunner,
) *ScheduleRefreshProcessor {
	return &ScheduleRefreshProcessor{
		db:      db,
		lockMgr: lockMgr,
		runner:  runner,
		parser: cron.NewParser(
			cron.SecondOptional |
				cron.Minute |
				cron.Hour |
				cron.Dom |
				cron.Month |
				cron.Dow |
				cron.Descriptor,
		),
	}
}

// Process processes a scheduled document refresh with the given lease.
// 单次调度执行的主流程：启动心跳 -> 加载 schedule/doc -> 创建执行记录 ->
// 调用文档刷新 runner -> 写执行结果和下次运行时间。
func (p *ScheduleRefreshProcessor) Process(ctx context.Context, lease *ScheduleLockLease) {
	heartbeat := p.lockMgr.StartHeartbeat(lease)
	defer heartbeat.Stop()

	// Get schedule info
	var schedule entity.KnowledgeDocumentScheduleDO
	if err := p.db.Where("id = ? AND deleted = 0", lease.ScheduleID).First(&schedule).Error; err != nil {
		slog.Warn("schedule not found", "scheduleID", lease.ScheduleID, "err", err)
		return
	}

	var doc entity.KnowledgeDocumentDO
	if err := p.db.Where("id = ? AND deleted = 0", schedule.DocID).First(&doc).Error; err != nil {
		// 文档不存在时禁用 schedule，避免调度器每轮扫描都重复失败。
		p.disableSchedule(schedule.ID, "document missing")
		slog.Warn("document not found", "docID", schedule.DocID, "err", err)
		return
	}

	if !p.shouldRun(doc, schedule) {
		// 文档被禁用、非 URL 来源或 cron 被清空时，schedule 不再具备执行条件。
		p.disableSchedule(schedule.ID, "schedule disabled")
		return
	}

	execID := p.createExec(schedule, doc)
	if strings.EqualFold(doc.Status, "running") {
		// 文档已经在手动分块或其他调度中运行时，本次调度跳过，避免并发刷新同一文档。
		p.finishExec(execID, "skipped", "document already running")
		p.updateScheduleState(schedule, "skipped", "")
		return
	}

	if p.runner == nil {
		p.finishExec(execID, "failed", "runner is not configured")
		p.updateScheduleState(schedule, "failed", "runner is not configured")
		return
	}

	if err := p.runner.RefreshDocument(ctx, doc.ID); err != nil {
		// RefreshDocument 内部会更新文档状态和 chunk 日志；这里记录调度执行层面的失败。
		p.finishExec(execID, "failed", err.Error())
		p.updateScheduleState(schedule, "failed", err.Error())
		slog.Error("scheduled document refresh failed",
			"scheduleID", lease.ScheduleID,
			"docID", schedule.DocID,
			"err", err)
		return
	}

	p.finishExec(execID, "success", "")
	p.updateScheduleState(schedule, "success", "")
	slog.Info("scheduled document refresh completed",
		"scheduleID", lease.ScheduleID,
		"docID", schedule.DocID)
}

// shouldRun 判断 schedule 当前是否仍可执行。
// 只有 URL 文档能定时重新拉取；文档和 schedule 都启用且 cron 非空才放行。
func (p *ScheduleRefreshProcessor) shouldRun(doc entity.KnowledgeDocumentDO, schedule entity.KnowledgeDocumentScheduleDO) bool {
	return strings.EqualFold(strings.TrimSpace(doc.SourceType), "url") &&
		doc.Enabled == 1 &&
		schedule.Enabled == 1 &&
		strings.TrimSpace(schedule.CronExpr) != ""
}

// createExec 创建一条调度执行记录，用于审计每次刷新开始、结束和结果。
func (p *ScheduleRefreshProcessor) createExec(schedule entity.KnowledgeDocumentScheduleDO, doc entity.KnowledgeDocumentDO) string {
	exec := entity.KnowledgeDocumentScheduleExecDO{
		BaseModel:  entity.BaseModel{ID: schedule.ID + "-" + time.Now().Format("20060102150405.000000000")},
		ScheduleID: schedule.ID,
		DocID:      doc.ID,
		KBID:       doc.KBID,
		Status:     "running",
		StartTime:  ptrTime(time.Now()),
	}
	_ = p.db.Create(&exec).Error
	return exec.ID
}

// finishExec 补全调度执行记录的结束状态。
func (p *ScheduleRefreshProcessor) finishExec(execID, status, message string) {
	if strings.TrimSpace(execID) == "" {
		return
	}
	p.db.Model(&entity.KnowledgeDocumentScheduleExecDO{}).Where("id = ? AND deleted = 0", execID).Updates(map[string]interface{}{
		"status":   status,
		"message":  message,
		"end_time": time.Now(),
	})
}

// updateScheduleState 更新 schedule 的最近运行状态，并计算下一次运行时间。
// 同时清理 lock_until，表示本次持有的数据库锁已经结束。
func (p *ScheduleRefreshProcessor) updateScheduleState(schedule entity.KnowledgeDocumentScheduleDO, status, message string) {
	updates := map[string]interface{}{
		"last_run_time": time.Now(),
		"last_status":   status,
		"last_error":    message,
		"lock_until":    nil,
	}
	if status == "success" {
		updates["last_success_time"] = time.Now()
	}
	if strings.TrimSpace(schedule.CronExpr) != "" {
		if nextRun, err := p.nextRunTime(schedule.CronExpr); err == nil {
			updates["next_run_time"] = nextRun
		}
	}
	_ = p.db.Model(&entity.KnowledgeDocumentScheduleDO{}).Where("id = ? AND deleted = 0", schedule.ID).Updates(updates).Error
}

// disableSchedule 禁用不可继续执行的 schedule，例如文档已删除或配置失效。
func (p *ScheduleRefreshProcessor) disableSchedule(scheduleID, message string) {
	_ = p.db.Model(&entity.KnowledgeDocumentScheduleDO{}).Where("id = ? AND deleted = 0", scheduleID).Updates(map[string]interface{}{
		"enabled":       0,
		"next_run_time": nil,
		"last_status":   "disabled",
		"last_error":    message,
		"lock_until":    nil,
	}).Error
}

// nextRunTime 根据 cron 表达式计算下一次触发时间。
func (p *ScheduleRefreshProcessor) nextRunTime(cronExpr string) (time.Time, error) {
	schedule, err := p.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(time.Now()), nil
}

// ptrTime 返回 time 指针，便于填充可空时间字段。
func ptrTime(t time.Time) *time.Time {
	return &t
}

// DocumentStatusHelper helps manage document status.
type DocumentStatusHelper struct {
	db *gorm.DB
}

// NewDocumentStatusHelper creates a new status helper.
func NewDocumentStatusHelper(db *gorm.DB) *DocumentStatusHelper {
	return &DocumentStatusHelper{db: db}
}

// UpdateStatus updates the status of a document.
func (h *DocumentStatusHelper) UpdateStatus(docID string, status string) {
	h.db.Exec(`
		UPDATE t_knowledge_document 
		SET status = ?, update_time = NOW()
		WHERE id = ? AND deleted = 0
	`, status, docID)
}

// RecoverStuckRunning recovers documents stuck in RUNNING status.
// 如果进程崩溃导致文档长期停留 RUNNING，定时任务会把它们标记为 FAILED，
// 让用户或调度器可以重新触发，而不是永久卡住。
func (h *DocumentStatusHelper) RecoverStuckRunning(timeoutMinutes int64) int {
	cutoff := time.Now().Add(-time.Duration(timeoutMinutes) * time.Minute)

	result := h.db.Exec(`
		UPDATE t_knowledge_document 
		SET status = 'FAILED', update_time = NOW()
		WHERE status = 'RUNNING' AND update_time < ? AND deleted = 0
	`, cutoff)

	if result.Error != nil {
		slog.Warn("recover stuck documents failed", "err", result.Error)
		return 0
	}

	return int(result.RowsAffected)
}
