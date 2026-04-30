package scheduler

import (
	"fmt"
	"gogent/internal/entity"
	"gogent/pkg/idgen"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// document_schedule_service.go 是文档调度的领域服务层。
// 它管理 knowledge_document_schedule 表，负责调度配置的持久化同步。
//
// 核心方法：
//   - UpsertSchedule：文档上传/更新/手动 chunk 时调用，允许"首次创建 schedule"
//   - SyncScheduleIfExists：文档普通信息更新时调用，仅同步已存在的 schedule，不主动创建
//   - DeleteByDocID：文档删除时级联清理 schedule 和执行历史（事务保证原子性）
//
// syncSchedule 内部逻辑：
//  1. 只允许 URL 来源文档（文件无法重新拉取）
//  2. 文档被禁用时强制关闭 schedule（docEnabled = false）
//  3. enabled=true 且 cron 非空时计算 next_run_time 写入表，供 schedule_job 扫描
//  4. enabled=false 时 next_run_time 留 NULL，schedule_job 不会扫到该行
//
// computeNextRunTime 连续调用两次 schedule.Next 校验 cron 可持续触发，
// 同时检查 minIntervalSeconds 防止频率过高。
type KnowledgeDocumentScheduleService struct {
	db                 *gorm.DB
	minIntervalSeconds int64
	parser             cron.Parser
}

// NewKnowledgeDocumentScheduleService 创建文档调度领域服务。
// minIntervalSeconds 用来限制 cron 最小触发间隔，防止配置过密导致重复入库。
func NewKnowledgeDocumentScheduleService(db *gorm.DB, minIntervalSeconds int64) *KnowledgeDocumentScheduleService {
	if minIntervalSeconds <= 0 {
		minIntervalSeconds = 60
	}
	return &KnowledgeDocumentScheduleService{
		db:                 db,
		minIntervalSeconds: minIntervalSeconds,
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

// UpsertSchedule 根据文档配置创建或更新调度记录。
func (s *KnowledgeDocumentScheduleService) UpsertSchedule(document entity.KnowledgeDocumentDO) error {
	return s.syncSchedule(document, true)
}

// SyncScheduleIfExists 只同步已存在的调度记录，不主动创建。
// 适合文档普通更新时保持已有 schedule 状态，而不是给所有文档自动建调度。
func (s *KnowledgeDocumentScheduleService) SyncScheduleIfExists(document entity.KnowledgeDocumentDO) error {
	return s.syncSchedule(document, false)
}

// DeleteByDocID 删除文档时同步清理 schedule 和执行历史。
func (s *KnowledgeDocumentScheduleService) DeleteByDocID(docID string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("doc_id = ?", docID).Delete(&entity.KnowledgeDocumentScheduleExecDO{}).Error; err != nil {
			return err
		}
		return tx.Where("doc_id = ?", docID).Delete(&entity.KnowledgeDocumentScheduleDO{}).Error
	})
}

// syncSchedule 将 KnowledgeDocumentDO 上的调度字段同步到 schedule 表。
// 只有 URL 文档支持定时刷新；文件上传文档没有远端来源可重新拉取。
func (s *KnowledgeDocumentScheduleService) syncSchedule(document entity.KnowledgeDocumentDO, allowCreate bool) error {
	if s.db == nil || strings.TrimSpace(document.ID) == "" || strings.TrimSpace(document.KBID) == "" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(document.SourceType), "url") {
		return nil
	}

	enabled := document.ScheduleEnabled == 1
	docEnabled := document.Enabled == 0 || document.Enabled == 1
	if document.Enabled == 0 {
		// 文档被禁用时，调度也必须关闭，避免刷新不可用文档。
		docEnabled = false
	}
	cronExpr := strings.TrimSpace(document.ScheduleCron)
	if cronExpr == "" {
		// cron 为空等价于未启用调度。
		enabled = false
	}
	if !docEnabled {
		enabled = false
	}

	var nextRunTime *time.Time
	if enabled && cronExpr != "" {
		// 只有真正启用时才计算 next_run_time；禁用状态下保持 NULL。
		next, err := s.computeNextRunTime(cronExpr)
		if err != nil {
			return err
		}
		nextRunTime = &next
	}

	var existing entity.KnowledgeDocumentScheduleDO
	err := s.db.Where("doc_id = ? AND deleted = 0", document.ID).First(&existing).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}
	if err == gorm.ErrRecordNotFound {
		if !allowCreate {
			return nil
		}
		// 第一次开启调度时创建 schedule 主记录。
		return s.db.Create(&entity.KnowledgeDocumentScheduleDO{
			BaseModel:   entity.BaseModel{ID: idgen.NextIDStr()},
			DocID:       document.ID,
			KBID:        document.KBID,
			CronExpr:    cronExpr,
			Enabled:     entity.FlexEnabled(boolToInt(enabled)),
			NextRunTime: nextRunTime,
		}).Error
	}
	nextRunValue := interface{}(gorm.Expr("NULL"))
	if nextRunTime != nil {
		nextRunValue = *nextRunTime
	}
	// 已存在时只同步 cron、启用状态和下一次运行时间，保留执行历史。
	return s.db.Model(&entity.KnowledgeDocumentScheduleDO{}).
		Where("id = ? AND deleted = 0", existing.ID).
		Updates(map[string]interface{}{
			"cron_expr":     cronExpr,
			"enabled":       boolToInt(enabled),
			"next_run_time": nextRunValue,
		}).Error
}

// computeNextRunTime 校验 cron 并返回下一次触发时间。
// 通过连续两次 Next 判断表达式是否可持续触发，并验证最小间隔。
func (s *KnowledgeDocumentScheduleService) computeNextRunTime(cronExpr string) (time.Time, error) {
	schedule, err := s.parser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("定时表达式不合法")
	}
	now := time.Now()
	first := schedule.Next(now)
	second := schedule.Next(first)
	if first.IsZero() || second.IsZero() {
		return time.Time{}, fmt.Errorf("定时表达式不合法")
	}
	if second.Sub(first) < time.Duration(s.minIntervalSeconds)*time.Second {
		return time.Time{}, fmt.Errorf("定时执行间隔不能小于%d秒", s.minIntervalSeconds)
	}
	return first, nil
}

// boolToInt 将 bool 转换为表结构使用的 0/1 enabled 值。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
