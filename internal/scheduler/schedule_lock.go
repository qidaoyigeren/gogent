package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// renewLockLua 原子性地校验锁归属并延长 TTL：GET key == lockID ? PEXPIRE : return 0。
var renewLockLua = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

// ScheduleLockLease represents a distributed lock lease.
type ScheduleLockLease struct {
	ScheduleID string
	LockID     string
	LockedAt   time.Time
	LockUntil  time.Time
}

// ScheduleLockHeartbeat manages heartbeat for a lock lease.
type ScheduleLockHeartbeat struct {
	lockMgr *ScheduleLockManager
	lease   *ScheduleLockLease
	ctx     context.Context
	cancel  context.CancelFunc
	stopCh  chan struct{}
	stopped bool
	mu      sync.Mutex
}

// ScheduleLockManager manages distributed locks for scheduled tasks.
// Mirrors Java ScheduleLockManager with lock acquisition and heartbeat.
type ScheduleLockManager struct {
	rdb         *redis.Client
	db          *gorm.DB
	lockSeconds int64 // Lock duration in seconds
}

// NewScheduleLockManager creates a new lock manager.
func NewScheduleLockManager(rdb *redis.Client, db *gorm.DB, lockSeconds int64) *ScheduleLockManager {
	if lockSeconds <= 0 {
		lockSeconds = 300 // Default 5 minutes
	}
	return &ScheduleLockManager{
		rdb:         rdb,
		db:          db,
		lockSeconds: lockSeconds,
	}
}

// TryAcquire attempts to acquire a lock for a schedule.
// Returns nil if lock cannot be acquired.
// 分布式锁采用“双层保护”：Redis SetNX 用于跨实例快速互斥，数据库 lock_until 用于
// 可见性和 Redis 不可用时的兜底。只有两层都成功，才认为本实例拿到执行权。
func (m *ScheduleLockManager) TryAcquire(ctx context.Context, scheduleID string, now time.Time) *ScheduleLockLease {
	lockKey := fmt.Sprintf("schedule:lock:%s", scheduleID)
	lockID := fmt.Sprintf("lock:%d", now.UnixNano())
	lockUntil := now.Add(time.Duration(m.lockSeconds) * time.Second)

	if m.rdb != nil {
		acquired, err := m.rdb.SetNX(ctx, lockKey, lockID, time.Duration(m.lockSeconds)*time.Second).Result()
		if err != nil {
			slog.Warn("redis lock failed", "scheduleID", scheduleID, "err", err)
		} else if !acquired {
			return nil
		}
	}

	result := m.db.Exec(`
		UPDATE t_knowledge_document_schedule 
		SET lock_until = ?, update_time = NOW()
		WHERE id = ? AND deleted = 0 
		  AND (lock_until IS NULL OR lock_until < ?)
	`, lockUntil, scheduleID, now)

	if result.Error != nil {
		slog.Warn("db lock failed", "scheduleID", scheduleID, "err", result.Error)
		if m.rdb != nil {
			m.rdb.Del(ctx, lockKey)
		}
		return nil
	}

	if result.RowsAffected == 0 {
		if m.rdb != nil {
			// DB 没抢到时释放前面拿到的 Redis 锁，避免 Redis 和 DB 状态不一致。
			m.rdb.Del(ctx, lockKey)
		}
		return nil
	}

	return &ScheduleLockLease{
		ScheduleID: scheduleID,
		LockID:     lockID,
		LockedAt:   now,
		LockUntil:  lockUntil,
	}
}

// StartHeartbeat starts a heartbeat goroutine to extend the lock periodically.
// 刷新任务可能超过 lockSeconds，因此需要心跳持续延长 lease。
func (m *ScheduleLockManager) StartHeartbeat(lease *ScheduleLockLease) *ScheduleLockHeartbeat {
	ctx, cancel := context.WithCancel(context.Background())
	hb := &ScheduleLockHeartbeat{
		lockMgr: m,
		lease:   lease,
		ctx:     ctx,
		cancel:  cancel,
		stopCh:  make(chan struct{}),
	}

	go hb.run()

	return hb
}

// Release releases a lock lease.
// 释放时同时清 Redis 和 DB，确保下次扫描可以重新处理该 schedule。
func (m *ScheduleLockManager) Release(ctx context.Context, lease *ScheduleLockLease) {
	if lease == nil {
		return
	}

	lockKey := fmt.Sprintf("schedule:lock:%s", lease.ScheduleID)

	// Release Redis lock
	if m.rdb != nil {
		m.rdb.Del(ctx, lockKey)
	}

	// Release DB lock
	m.db.Exec(`
		UPDATE t_knowledge_document_schedule 
		SET lock_until = NULL, update_time = NOW()
		WHERE id = ? AND deleted = 0
	`, lease.ScheduleID)

	slog.Debug("schedule lock released", "scheduleID", lease.ScheduleID)
}

func (hb *ScheduleLockHeartbeat) run() {
	internal := time.Duration(hb.lockMgr.lockSeconds/2) * time.Second
	ticker := time.NewTicker(internal)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !hb.renew() {
				return
			}
		case <-hb.stopCh:
			return
		}
	}
}

// renew extends the lock.
// 先通过 Lua 脚本原子性校验 Redis 锁归属并延长 TTL，再续期 DB。
// 任一失败都清理另一层，保持 Redis/DB 双层状态一致。
func (hb *ScheduleLockHeartbeat) renew() bool {
	hb.mu.Lock()
	defer hb.mu.Unlock()

	if hb.stopped {
		return false
	}

	now := time.Now()
	newLockUntil := now.Add(time.Duration(hb.lockMgr.lockSeconds) * time.Second)
	lockKey := fmt.Sprintf("schedule:lock:%s", hb.lease.ScheduleID)
	lockTTLms := hb.lockMgr.lockSeconds * 1000

	if hb.lockMgr.rdb != nil {
		result, err := renewLockLua.Run(hb.ctx, hb.lockMgr.rdb, []string{lockKey}, hb.lease.LockID, lockTTLms).Result()
		if err != nil || result == 0 {
			slog.Warn("heartbeat redis renewal failed, releasing lock",
				"scheduleID", hb.lease.ScheduleID, "err", err)
			// Redis 续期失败（锁已过期或不属于自己），清理 DB 锁
			hb.lockMgr.db.Exec(`
				UPDATE t_knowledge_document_schedule
				SET lock_until = NULL, update_time = NOW()
				WHERE id = ? AND deleted = 0
			`, hb.lease.ScheduleID)
			return false
		}
	}
	//DB续期lock_until
	result := hb.lockMgr.db.Exec(`
		UPDATE t_knowledge_document_schedule
		SET lock_until = ?, update_time = NOW()
		WHERE id = ? AND deleted = 0 AND lock_until > ?
	`, newLockUntil, hb.lease.ScheduleID, now)

	if result.Error != nil || result.RowsAffected == 0 {
		slog.Warn("heartbeat db renewal failed, releasing redis lock",
			"scheduleID", hb.lease.ScheduleID)
		// DB 续期失败，清理 Redis 锁
		if hb.lockMgr.rdb != nil {
			hb.lockMgr.rdb.Del(hb.ctx, lockKey)
		}
		return false
	}

	hb.lease.LockUntil = newLockUntil
	slog.Debug("heartbeat renewed", "scheduleID", hb.lease.ScheduleID, "lockUntil", newLockUntil)
	return true
}

// Stop stops the heartbeat and releases the lock.
// Processor defer 调用 Stop，保证成功、失败或提前返回都会释放锁。
func (hb *ScheduleLockHeartbeat) Stop() {
	hb.mu.Lock()
	defer hb.mu.Unlock()

	if hb.stopped {
		return
	}

	hb.stopped = true
	hb.cancel()
	close(hb.stopCh)

	// Release the lock
	hb.lockMgr.Release(context.Background(), hb.lease)
}

// IsStopped returns whether the heartbeat is stopped.
func (hb *ScheduleLockHeartbeat) IsStopped() bool {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return hb.stopped
}
