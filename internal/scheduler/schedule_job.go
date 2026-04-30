package scheduler

// 本文件是**调度闭环的定时器与线程池**
//
// 三线程模型（理解 main 里 defer Stop 的语义）：
//   1) scanJob：每 scanDelaySec 秒拉一批「到期且未锁或锁已过期」的 schedule，TryAcquire 成功后把刷新闭包丢进 executor 队列
//   2) executorWorker（共 batchSize 个）：从 channel 取闭包执行 → 实际进入 ScheduleRefreshProcessor.Process
//   3) recoveryJob：每 60s 调 DocumentStatusHelper，把长时间 RUNNING 的文档打回失败，防止崩溃遗留（与 document_recovery 理念一致）
//
// 反压：executor channel 满时**必须 Release 锁**，否则该 schedule 要等租约到期才能再次被扫描到。

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gogent/internal/entity"

	"gorm.io/gorm"
)

// KnowledgeDocumentScheduleJob 知识文档定时调度任务。
//  1. scanJob    —— 周期性扫描到期的知识文档调度记录，获取分布式锁后投入执行队列
//  2. recoveryJob —— 周期性恢复长时间处于 RUNNING 状态的文档，防止进程崩溃遗留的僵尸状态
//
// 整体采用「扫描 + 工作池」的生产者-消费者模型，通过 executor channel 做反压控制。
type KnowledgeDocumentScheduleJob struct {
	db           *gorm.DB
	lockMgr      *ScheduleLockManager
	refreshProc  *ScheduleRefreshProcessor
	scanDelaySec int
	batchSize    int
	executor     chan func() // 任务载体是闭包，内部捕获 lease 并调用 refreshProc.Process
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// NewKnowledgeDocumentScheduleJob 创建知识文档定时调度任务实例。
//
// 参数说明：
//   - db:          数据库连接实例，用于查询调度记录和更新文档状态
//   - lockMgr:     分布式锁管理器，确保同一调度记录在同一时刻只被一个实例处理
//   - refreshProc: 调度刷新处理器，实际执行文档向量化等耗时操作
//   - scanDelaySec: 扫描间隔（秒），控制 scanJob 的轮询频率，默认 10 秒
//   - batchSize:    批处理大小，同时也是 executor 工作协程的数量，默认 5
func NewKnowledgeDocumentScheduleJob(
	db *gorm.DB,
	lockMgr *ScheduleLockManager,
	refreshProc *ScheduleRefreshProcessor,
	scanDelaySec int,
	batchSize int,
) *KnowledgeDocumentScheduleJob {
	if scanDelaySec <= 0 {
		scanDelaySec = 10
	}
	if batchSize <= 0 {
		batchSize = 5
	}

	// executor 缓冲区比 batchSize 稍大，允许扫描线程短暂领先 worker。
	return &KnowledgeDocumentScheduleJob{
		db:           db,
		lockMgr:      lockMgr,
		refreshProc:  refreshProc,
		scanDelaySec: scanDelaySec,
		batchSize:    batchSize,
		executor:     make(chan func(), batchSize*2),
		stopCh:       make(chan struct{}),
	}
}

// Start 启动所有定时调度任务。
// 依次启动 executor 工作协程池、恢复任务和扫描任务。
// 通常在应用初始化完成后调用，配合 defer Stop() 确保优雅退出。
func (j *KnowledgeDocumentScheduleJob) Start(ctx context.Context) {
	// 启动 executor 工作协程池。
	// worker 数量等于 batchSize，限制同一实例内并发刷新文档的数量，防止过载。
	for i := 0; i < j.batchSize; i++ {
		j.wg.Add(1)
		go j.executorWorker()
	}

	// 启动恢复任务：定期清理僵尸 RUNNING 状态的文档
	j.wg.Add(1)
	go j.recoveryJob()

	// 启动扫描任务：定期扫描到期的调度记录并投入执行队列
	j.wg.Add(1)
	go j.scanJob()

	slog.Info("knowledge document schedule job started",
		"scanDelaySec", j.scanDelaySec,
		"batchSize", j.batchSize)
}

// Stop 优雅停止所有定时调度任务。
// 关闭顺序：先通知 scanJob/recoveryJob 退出 → 关闭 executor channel → 等待所有 worker 结束。
// 顺序至关重要：若先关闭 executor，scanJob 可能仍在投递任务导致 panic（写入已关闭的 channel）。
func (j *KnowledgeDocumentScheduleJob) Stop() {
	// 先通知扫描/恢复退出，再关闭 executor，最后等待所有 worker 结束。
	// 顺序很关键：若先关 executor，scanJob 仍可能投递任务导致 panic。
	close(j.stopCh)
	close(j.executor)
	j.wg.Wait()
	slog.Info("knowledge document schedule job stopped")
}

// executorWorker 从 executor channel 中取出闭包任务并执行。
// 每个 worker 协程独立运行，当 channel 被关闭且消费完毕后自动退出。
// 不做 panic recover：让异常快速暴露，便于排查问题；调度的稳定性依赖上层 Process 自身的错误处理。
func (j *KnowledgeDocumentScheduleJob) executorWorker() {
	defer j.wg.Done()

	for task := range j.executor {
		// 不 recover panic：让异常尽快暴露；调度稳定性依赖上层 Process 自身错误处理。
		task()
	}
}

// recoveryJob 定期恢复长时间卡在 RUNNING 状态的文档。
// 对应 Java 端 recoverStuckRunningDocuments() 方法，每 60 秒执行一次。
// 典型场景：进程崩溃或网络中断导致文档状态未能正常更新，此任务将其标记为失败以便重新调度。
func (j *KnowledgeDocumentScheduleJob) recoveryJob() {
	defer j.wg.Done()

	// 启动后延迟 30 秒再开始恢复，避免服务刚启动时与历史任务/worker 初始化产生状态竞争。
	select {
	case <-time.After(30 * time.Second):
	case <-j.stopCh:
		return
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			j.recoverStuckRunning()
		case <-j.stopCh:
			return
		}
	}
}

// scanJob 定期扫描需要处理的调度记录。
// 对应 Java 端 scan() 方法，每 scanDelaySec 秒执行一次。
// 核心逻辑：查询到期且未被锁定的调度记录，获取分布式锁后投入 executor 工作池执行。
func (j *KnowledgeDocumentScheduleJob) scanJob() {
	defer j.wg.Done()

	// 延迟首次扫描，给 Redis、DB、HTTP 路由和文档 worker 等基础设施的初始化留出充足时间。
	select {
	case <-time.After(time.Duration(j.scanDelaySec) * time.Second):
	case <-j.stopCh:
		return
	}

	ticker := time.NewTicker(time.Duration(j.scanDelaySec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			j.scanAndProcess()
		case <-j.stopCh:
			return
		}
	}
}

// recoverStuckRunning 执行实际的僵尸文档恢复操作。
// 将超过 timeoutMinutes（默认 30 分钟）仍处于 RUNNING 状态的文档标记为失败，
// 使其可以被重新调度执行，避免因进程异常退出导致文档永远卡在处理中状态。
func (j *KnowledgeDocumentScheduleJob) recoverStuckRunning() {
	timeoutMinutes := int64(30) // 默认超时时间：30 分钟
	recovered := NewDocumentStatusHelper(j.db).RecoverStuckRunning(timeoutMinutes)
	if recovered > 0 {
		slog.Warn("recovered stuck documents",
			"count", recovered,
			"timeoutMinutes", timeoutMinutes)
	}
}

// scanAndProcess 扫描到期的调度记录并提交给工作池处理。
// 核心流程：
//  1. 从数据库查询已启用、已到期、未被锁定的调度记录（按 next_run_time 升序取 batchSize 条）
//  2. 对每条记录尝试获取分布式锁（TryAcquire）
//  3. 获取成功则将刷新闭包投入 executor channel；获取失败则跳过（其他实例正在处理）
//  4. 若 executor channel 已满（反压），立即释放锁，避免该记录被锁住直到租约过期
func (j *KnowledgeDocumentScheduleJob) scanAndProcess() {
	ctx := context.Background()
	now := time.Now()

	// 查询需要处理的调度记录。
	// 查询条件：已启用(enabled) + 未删除(deleted=0) + 已到期(next_run_time<=now) + 未被锁定(lock_until<now)。
	// 按 next_run_time 升序排列，优先处理最早到期的记录，限制返回 batchSize 条。
	var schedules []entity.KnowledgeDocumentScheduleDO
	err := j.db.Where(`
		CAST(enabled AS TEXT) IN ('1','true','t')
		AND deleted = 0
		AND (next_run_time IS NULL OR next_run_time <= ?)
		AND (lock_until IS NULL OR lock_until < ?)
		ORDER BY next_run_time ASC
		LIMIT ?
	`, now, now, j.batchSize).Find(&schedules).Error

	if err != nil {
		slog.Warn("schedule scan failed", "err", err)
		return
	}

	if len(schedules) == 0 {
		return
	}

	slog.Info("schedules found", "count", len(schedules))

	for _, schedule := range schedules {
		lease := j.lockMgr.TryAcquire(ctx, schedule.ID, now)
		if lease == nil {
			// 未获取到锁，说明其他实例或协程正在处理该调度记录，跳过即可。
			continue
		}

		// 将刷新任务提交到 executor 工作池。
		// 使用 select + default 实现非阻塞投递：channel 满时走 default 分支释放锁。
		select {
		case j.executor <- func() {
			// 闭包捕获了 lease 变量，lease 的生命周期（包括分布式锁续期和释放）
			// 由 Process 内部的 heartbeat.Stop() 负责管理。
			j.refreshProc.Process(ctx, lease)
		}:
		default:
			// executor 队列已满，触发反压机制，必须主动释放锁。
			// 若不释放，该调度记录要等到 lock_until 过期才能再次被扫描到，造成不必要的延迟。
			j.lockMgr.Release(ctx, lease)
			slog.Warn("executor full, skipping schedule", "scheduleID", schedule.ID)
		}
	}
}
