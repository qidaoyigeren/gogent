package service

import (
	"context"
	"fmt"
	"gogent/internal/mq"
	"gogent/pkg/idgen"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ========================= 场景 =========================
// RAG 问答极度吃 LLM/向量库资源，必须做"全局并发上限"。直接拒绝体验太差，
// 所以采用 "排队 + 信号量" 的组合：
//   - 信号量 slot 数 = MaxConcurrent（允许同时在跑的请求数）
//   - 超过 slot 则进队列，按 FIFO 顺序等待；超过 MaxWaitSeconds 就返回限流错误
// chat_handler 在 Acquire 成功后 defer Release，保证槽位一定归还。
//
// ========================= Redis 数据结构 =========================
// semaphoreKey   (SET)  ：当前持有 lease 的集合；SCARD 得到已占用槽位数
// queueKey       (ZSET) ：请求 ID 按 score 升序排队；score = 单调递增序号（FIFO）
// queueSeqKey    (STRING INCR) ：单调序号源；配合 ZADD 保证入队顺序稳定
//
// ========================= NATS =========================
// 释放 slot 时通过 NATS 发消息，让排队中的协程立即重试（降低 polling 时延）
//
// ========================= 两把原子 Lua =========================
// claimQueueHeadLua  ：原子判定"我是不是队头 N 个之一" + ZREM，避免多个实例误判
// tryAcquire 脚本    ：原子 SCARD 检查 + SADD，避免超卖槽位
//
// ========================= 容错 =========================
// Enabled=false 时 Acquire/Release 全部 no-op（返回空 leaseID），兼容本地开发无 Redis 的情况。
// NATS 不可用时降级为纯轮询（polling ticker 兜底）。
// leaseSeconds 给 SET 加 TTL，防止宿主宕机导致 slot 被永久占住。

// 行为：ZRANK(queue, requestId) < maxRank → 视为"队头 N 个"，原子 ZREM 拿走 score；否则返回 {0}。
// 返回 {1, score} 表示成功 claim；{0} 表示没轮到或已被别人抢走。
// 原子性重点：ZRANK + ZREM 必须在一次 Lua 内完成，否则会出现"A 看到自己是队头，但 B 也同时看到"的双主问题。
const claimQueueHeadLua = `
local queueKey = KEYS[1]
local requestId = ARGV[1]
local maxRank = tonumber(ARGV[2])
local rank = redis.call('ZRANK', queueKey, requestId)
if not rank then return {0} end
if rank >= maxRank then return {0} end
local score = redis.call('ZSCORE', queueKey, requestId)
redis.call('ZREM', queueKey, requestId)
return {1, score}
`

// RateLimiter 实现分布式限流：
//  1. 每个请求生成唯一 requestID → ZADD 进队（score 单调递增保证 FIFO）
//  2. 循环：检测可用 slot → 判定是否队头 → 原子拿 slot
//  3. 成功返回 leaseID；Release 时 SRem slot 并通过 NATS publish 通知下一位
type RateLimiter struct {
	rdb  *redis.Client
	nats *mq.NATSPublisher
	cfg  RateLimitConfig

	// Redis key 统一在构造时拼好，方便后续换前缀
	semaphoreKey string
	queueKey     string
	queueSeqKey  string
	natsSubject  string // NATS subject for slot-release notifications

	stopOnce sync.Once     // 保证 Stop 幂等
	stopCh   chan struct{} // 关闭时通知所有 Acquire 协程退出
}

// RateLimitConfig 从 application.yaml 的 rag.rate-limit.global.* 绑定。
// mapstructure 标签用于 viper.Unmarshal；所有 *Seconds / *Ms 都有合理默认值。
type RateLimitConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	MaxConcurrent  int  `mapstructure:"max-concurrent"`   // 同时在跑的请求上限
	MaxWaitSeconds int  `mapstructure:"max-wait-seconds"` // 排队最长等待；超时返回错误
	LeaseSeconds   int  `mapstructure:"lease-seconds"`    // slot 的 TTL，防宿主宕机永不释放
	PollIntervalMs int  `mapstructure:"poll-interval-ms"` // nats 通知兜底的轮询周期
}

// NewRateLimiter 填默认值后构造实例。默认组合：10 并发、30s 等待、30s 租约、200ms 轮询。
func NewRateLimiter(rdb *redis.Client, natsPub *mq.NATSPublisher, cfg RateLimitConfig) *RateLimiter {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 10
	}
	if cfg.MaxWaitSeconds <= 0 {
		cfg.MaxWaitSeconds = 30
	}
	if cfg.LeaseSeconds <= 0 {
		cfg.LeaseSeconds = 30
	}
	if cfg.PollIntervalMs <= 0 {
		cfg.PollIntervalMs = 200
	}

	return &RateLimiter{
		rdb:          rdb,
		nats:         natsPub,
		cfg:          cfg,
		semaphoreKey: "ragent:ratelimit:semaphore",
		queueKey:     "ragent:ratelimit:queue",
		queueSeqKey:  "ragent:ratelimit:queue:seq",
		natsSubject:  "ragent.ratelimit.notify",
		stopCh:       make(chan struct{}),
	}
}

// Acquire 按严格 FIFO 排队获取 slot；成功返回 leaseID，超时/出错返回 err。
// 调用方用法：
//
//	leaseID, err := limiter.Acquire(ctx, userID)
//	if err != nil { 返回限流提示 }
//	defer limiter.Release(context.Background(), leaseID)
//
// userID 目前仅预留，未来可做按租户公平性；当前实现全局 FIFO。
func (l *RateLimiter) Acquire(ctx context.Context, userID string) (leaseID string, err error) {
	_ = userID
	// 未开启时直接放行，方便本地开发
	if !l.cfg.Enabled {
		return "", nil
	}

	// ---- 1) 入队：生成 requestID，拿到单调 seq 作为 score ----
	requestID := idgen.NextIDStr()
	seq, err := l.rdb.Incr(ctx, l.queueSeqKey).Result()
	if err != nil {
		return "", fmt.Errorf("queue seq incr: %w", err)
	}
	if err := l.rdb.ZAdd(ctx, l.queueKey, redis.Z{Score: float64(seq), Member: requestID}).Err(); err != nil {
		return "", fmt.Errorf("queue add failed: %w", err)
	}
	// 兜底清理：任何退出路径（超时/错误/成功）都尝试 ZRem。
	// 成功路径里 Lua 已经 ZRem 过了，这里是幂等保护；用 Background ctx 防 parent 取消后清理不掉。
	defer func() {
		_ = l.rdb.ZRem(context.Background(), l.queueKey, requestID).Err()
	}()

	// ---- 2) 为整次等待加一层 timeout ----
	timeout := time.Duration(l.cfg.MaxWaitSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// ---- 3) 订阅 NATS release 通知，降低 polling 间隔 ----
	notifyCtx, notifyCancel := context.WithCancel(ctx)
	defer notifyCancel()
	notifyCh := l.startNATSNotify(notifyCtx)
	defer notifyCh.close()

	// ---- 4) 轮询 ticker：NATS 通知兜底；生产中常见网络抖动场景 ----
	ticker := time.NewTicker(time.Duration(l.cfg.PollIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		// 先等唤醒事件：超时/停机/有 release/固定 poll
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("rate limit timeout")

		case <-l.stopCh:
			return "", fmt.Errorf("rate limiter stopped")

		case <-notifyCh.c:
		case <-ticker.C:
		}

		// 当前还有多少空 slot；没空继续等
		avail := l.availablePermits(ctx)
		if avail <= 0 {
			continue
		}

		// 判我是不是"队头 avail 个"之一；不是就继续等下一轮
		claimed, err := l.claimIfHead(ctx, requestID, avail)
		if err != nil {
			return "", err
		}
		if !claimed {
			continue
		}

		// 抢占 slot：claim 完可能被别人抢先消耗 slot（race），所以还要 Lua CAS
		leaseID, err = l.tryAcquire(ctx)
		if err == nil && leaseID != "" {
			return leaseID, nil
		}

		// claim 成功但抢 slot 失败：把自己重新入队（用新 seq，排到队尾，保证公平）
		// 对齐 Java 版本的重排策略
		newSeq, incErr := l.rdb.Incr(ctx, l.queueSeqKey).Result()
		if incErr != nil {
			return "", fmt.Errorf("queue seq re-incr: %w", incErr)
		}
		if err := l.rdb.ZAdd(ctx, l.queueKey, redis.Z{Score: float64(newSeq), Member: requestID}).Err(); err != nil {
			return "", fmt.Errorf("queue re-add failed: %w", err)
		}
		// 发通知让其他等待者重新尝试（因为 slot 刚又变紧）
		l.notifyAll()
	}
}

// availablePermits 返回当前剩余可用 slot 数；Redis 故障时保守返回 0。
func (l *RateLimiter) availablePermits(ctx context.Context) int {
	n, err := l.rdb.SCard(ctx, l.semaphoreKey).Result()
	if err != nil {
		return 0
	}
	avail := l.cfg.MaxConcurrent - int(n)
	if avail < 0 {
		return 0
	}
	return avail
}

// claimIfHead 通过 Lua 原子地尝试把自己从队头 avail 个窗口里拿走（ZRem）。
// 返回 true 表示"本请求获得出队资格"，但还需要继续抢 slot。
func (l *RateLimiter) claimIfHead(ctx context.Context, requestID string, avail int) (bool, error) {
	res, err := l.rdb.Eval(ctx, claimQueueHeadLua, []string{l.queueKey}, requestID, avail).Result()
	if err != nil {
		return false, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) == 0 {
		return false, nil
	}
	okVal := parseRedisLong(arr[0])
	return okVal == 1, nil
}

// parseRedisLong 将 Redis Eval 返回的多种整型兜底解析为 int64。
// go-redis 在不同版本/协议下可能返回 int/int64/string，要统一处理。
func parseRedisLong(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

// tryAcquire 用 Lua 原子 "SCARD 检查 + SADD" 抢一个 slot。
// 成功返回 leaseID（lease:<unix-nano>），失败返回错误。
// SET 本身带 EXPIRE（leaseSeconds）：宿主突然挂掉也不会让 slot 永占。
func (l *RateLimiter) tryAcquire(ctx context.Context) (string, error) {
	script := `
		local count = redis.call('SCARD', KEYS[1])
		if count < tonumber(ARGV[1]) then
			local lease_id = ARGV[2]
			redis.call('SADD', KEYS[1], lease_id)
			redis.call('EXPIRE', KEYS[1], ARGV[3])
			return lease_id
		end
		return nil
	`

	leaseID := fmt.Sprintf("lease:%d", time.Now().UnixNano())
	result, err := l.rdb.Eval(ctx, script, []string{l.semaphoreKey},
		l.cfg.MaxConcurrent, leaseID, l.cfg.LeaseSeconds).Result()

	if err != nil {
		return "", err
	}

	if result == nil {
		return "", fmt.Errorf("no available slots")
	}

	return result.(string), nil
}

// Release 归还 slot 并通过 NATS publish 通知下一个等待者。
// 用 Background ctx：即使业务 ctx 已 cancel，slot 也必须归还。
func (l *RateLimiter) Release(ctx context.Context, leaseID string) {
	if !l.cfg.Enabled || leaseID == "" {
		return
	}

	l.rdb.SRem(ctx, l.semaphoreKey, leaseID)
	l.notifyAll()
}

// notifyHandle 封装"通知 channel"+"关闭 channel"，上层 defer close 即可释放后台订阅协程。
type notifyHandle struct {
	c    <-chan struct{} // 收到 release 事件时 tick 一下
	done chan struct{}   // 主动关闭信号
}

func (h *notifyHandle) close() {
	close(h.done)
}

// startNATSNotify 订阅 NATS subject，把消息转成 struct{} 送入 out。
// out 是带缓冲的 non-blocking（默认 16 容量，满了丢弃），Acquire 轮询侧本来就会 polling 兜底，
// 丢消息不会导致饿死。
func (l *RateLimiter) startNATSNotify(parent context.Context) *notifyHandle {
	done := make(chan struct{})
	out := make(chan struct{}, 16)

	if l.nats == nil {
		// NATS 不可用时，返回空 handle，纯靠 polling ticker 兜底
		return &notifyHandle{c: out, done: done}
	}

	sub, err := l.nats.Subscribe(l.natsSubject, func(data []byte) {
		// non-blocking 发送：满了就丢
		select {
		case out <- struct{}{}:
		default:
		}
	})
	if err != nil {
		// 订阅失败，降级为纯轮询
		return &notifyHandle{c: out, done: done}
	}

	go func() {
		defer close(out)
		defer sub.Unsubscribe()
		<-done
	}()

	return &notifyHandle{c: out, done: done}
}

// notifyAll 向 NATS 广播"有 slot 释放了"；所有实例的等待协程都会被唤醒重试。
func (l *RateLimiter) notifyAll() {
	if l.nats == nil {
		return
	}
	_ = l.nats.PublishRaw(l.natsSubject, []byte("available"))
}

// Stop 由 main 在关停时调用，通知所有 Acquire 协程尽快退出。
// sync.Once 保证多次调用幂等。
func (l *RateLimiter) Stop() {
	l.stopOnce.Do(func() {
		close(l.stopCh)
	})
}

// GetQueueLength 返回当前排队长度，用于 /rag/settings 或监控指标展示。
func (l *RateLimiter) GetQueueLength(ctx context.Context) (int64, error) {
	return l.rdb.ZCard(ctx, l.queueKey).Result()
}

// GetAvailableSlots 返回可用 slot 数，用于监控 dashboard。
func (l *RateLimiter) GetAvailableSlots(ctx context.Context) (int, error) {
	return l.availablePermits(ctx), nil
}
