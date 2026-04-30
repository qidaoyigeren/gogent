package handler

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

//流式任务注册与分布式取消
////
//// ========================= 要解决的问题 =========================
//// 客户端点“停止生成”时：
////   1) 当前实例必须 context.Cancel，停掉编排（检索/LLM 都监听 ctx）；
////   2) 如果请求落在 A 实例、但 stop 打到 B 实例（负载均衡下常见），需要跨实例广播；
////   3) 即便 ctx 被 cancel，LLM HTTP 流可能还在读，需要显式调用 LLM SDK 的 stop 回调关闭底层连接。

// StreamTaskManager 管理所有活跃流式任务；单实例、单例模式。
// tasks 使用 sync.Map 是因为读远多于写（Cancel 才写），但任务量可能成千上万。
type StreamTaskManager struct {
	tasks sync.Map // key: taskID(string), value: *taskEntry
	rdb   *redis.Client
	ttl   time.Duration

	subOnce sync.Once // 保证订阅协程只启动一次
}

// taskEntry 代表一个注册中的流式任务。
type taskEntry struct {
	cancel    context.CancelFunc // 注册时传入：cancel 后编排 ctx 会立即唤醒 Done
	cancelled atomic.Bool        // CAS 防重复 cancel
	mu        sync.Mutex         // 保护 streamStop 字段的读写
	// streamStop 对应 Java StreamCancellationHandle：
	// 编排层拿到 LLM 流后会调 BindHandle 注册，cancel 时除了 ctx 还会显式关闭上游流。
	// 可为 nil（任务还没进入 LLM 阶段就被 cancel 的情况）。
	streamStop func()
}

// globalTaskManager 单例。main 里通过 GetTaskManager() 取，避免多处 new 出多个管理器导致分片。
var globalTaskManager = &StreamTaskManager{}

func GetTaskManager() *StreamTaskManager {
	return globalTaskManager
}

const (
	cancelTopic     = "ragent:stream:cancel"  // Pub/Sub 频道
	cancelKeyPrefix = "ragent:stream:cancel:" // Register 时检查的“已被取消”标记键前缀
)

// EnableDistributedCancel 启用分布式取消：
//   - 订阅 cancelTopic，收到 taskID 后本地 cancelLocal
//   - ttl 控制标记键的过期时间，默认 30min 保护性超时
//
// 多次调用安全：订阅只启动一次；rdb/ttl 可多次覆盖（生产中一般一次性设置）。
func (m *StreamTaskManager) EnableDistributedCancel(rdb *redis.Client, ttl time.Duration) {
	if rdb == nil {
		return
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	m.rdb = rdb
	m.ttl = ttl

	m.subOnce.Do(func() {
		sub := rdb.Subscribe(context.Background(), cancelTopic)
		go func() {
			// 后台长驻协程：进程存活期间一直订阅
			for msg := range sub.Channel() {
				taskID := msg.Payload
				if taskID == "" {
					continue
				}
				m.cancelLocal(taskID)
			}
		}()
	})
}

// Register 登记一个 taskID 及其 ctx 的 CancelFunc。
// 小心：chat_handler 在 Register 之后发 meta 事件前可能已经有别的实例 Publish 了 cancel，
// 所以这里额外去 Redis 查一次标记键，命中则立即本地 cancel。
func (m *StreamTaskManager) Register(taskID string, cancel context.CancelFunc) {
	m.tasks.Store(taskID, &taskEntry{cancel: cancel})
	if m.rdb != nil && taskID != "" {
		if v, err := m.rdb.Get(context.Background(), cancelKeyPrefix+taskID).Result(); err == nil && v != "" {
			m.cancelLocal(taskID)
		}
	}
}

// BindHandle 在流式 LLM 开始后注册“停流”回调；对齐 Java StreamTaskManager#bindHandle。
// 若任务在注册 stop 前已经被 cancel（边缘竞态），立即在当前 goroutine 调用 stop，保证不泄漏上游连接。
// stop 必须幂等：可能被本函数、cancelLocal 都触发一次。
func (m *StreamTaskManager) BindHandle(taskID string, stop func()) {
	if taskID == "" || stop == nil {
		return
	}
	v, ok := m.tasks.Load(taskID)
	if !ok {
		return
	}
	e := v.(*taskEntry)
	e.mu.Lock()
	e.streamStop = stop
	already := e.cancelled.Load()
	e.mu.Unlock()
	if already {
		stop()
	}
}

// Cancel 对外入口：由 /rag/v3/stop 路由或其他实例触发。
// 先写 Redis 标记键 + Publish（跨实例通知），然后本地 cancelLocal。
// 返回 true 表示本地确实有登记并且这是第一次 cancel；false 表示未注册或已 cancel。
func (m *StreamTaskManager) Cancel(taskID string) bool {
	if taskID == "" {
		return false
	}
	if m.rdb != nil {
		// 写标记：其他实例若在此之后才 Register，也能立即识别“这个 task 已被取消”
		_ = m.rdb.Set(context.Background(), cancelKeyPrefix+taskID, "1", m.ttl).Err()
		_ = m.rdb.Publish(context.Background(), cancelTopic, taskID).Err()
	}
	return m.cancelLocal(taskID)
}

// cancelLocal 仅在本实例内 cancel；通过 CAS 保证只执行一次。
// 顺序：先 streamStop（关闭 LLM 连接）再 cancel ctx（让编排 Goroutine 退出）。
// 为什么这个顺序：若 ctx cancel 在前，LLM HTTP 连接可能由 read goroutine 持有，
// 此时调 stop 能更快释放网络资源。
func (m *StreamTaskManager) cancelLocal(taskID string) bool {
	v, ok := m.tasks.Load(taskID)
	if !ok {
		return false
	}
	e := v.(*taskEntry)
	if !e.cancelled.CompareAndSwap(false, true) {
		return false
	}
	e.mu.Lock()
	stop := e.streamStop
	e.mu.Unlock()
	if stop != nil {
		stop()
	}
	e.cancel()
	return true
}

// Unregister 在 handler 结束时 defer 调用。
//   - 从本地 map 删除：防止 sync.Map 无限增长
//   - 删 Redis 标记键：后续无意义 Cancel 不再产生 cancelLocal（虽然 cancelLocal 本身也是幂等）
func (m *StreamTaskManager) Unregister(taskID string) {
	m.tasks.Delete(taskID)
	if m.rdb != nil && taskID != "" {
		_ = m.rdb.Del(context.Background(), cancelKeyPrefix+taskID).Err()
	}
}

// IsCancelled 用于外层（例如编排层）判断当前任务是否已被取消。
func (m *StreamTaskManager) IsCancelled(taskID string) bool {
	v, ok := m.tasks.Load(taskID)
	if !ok {
		return false
	}
	return v.(*taskEntry).cancelled.Load()
}
