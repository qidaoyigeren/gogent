package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	intentTreeCacheKey = "ragent:intent:tree" // Redis 缓存键
	intentTreeCacheTTL = 10 * time.Minute     // 缓存过期时间（10 分钟）
)

// TreeCache 意图树缓存（基于 Redis）
// 核心职责：
// 1. 缓存意图树到 Redis（TTL 10 分钟）
// 2. 缓存命中时直接返回，避免频繁查询数据库
// 3. 缓存未命中时从数据库加载并刷新缓存
// 4. 提供缓存失效接口（管理后台更新意图树时调用）
type TreeCache struct {
	rdb    *redis.Client
	loadFn func(ctx context.Context) ([]*IntentNode, error) // 数据库加载函数
}

var (
	defaultTreeCache   *TreeCache
	defaultTreeCacheMu sync.RWMutex
)

func NewTreeCache(rdb *redis.Client, loadFn func(ctx context.Context) ([]*IntentNode, error)) *TreeCache {
	c := &TreeCache{rdb: rdb, loadFn: loadFn}
	SetDefaultTreeCache(c)
	return c
}

func SetDefaultTreeCache(c *TreeCache) {
	defaultTreeCacheMu.Lock()
	defaultTreeCache = c
	defaultTreeCacheMu.Unlock()
}

func DefaultTreeCache() *TreeCache {
	defaultTreeCacheMu.RLock()
	defer defaultTreeCacheMu.RUnlock()
	return defaultTreeCache
}

// GetRoots 获取意图树根节点（优先使用 Redis 缓存）
// 工作流程：
// 1. 如果缓存为空或 loadFn 为空 → 返回空
// 2. 如果 Redis 为空 → 直接从数据库加载
// 3. 尝试从 Redis 获取缓存
// 4. 缓存命中 → 返回
// 5. 缓存未命中 → 从数据库加载并写入缓存
func (c *TreeCache) GetRoots(ctx context.Context) ([]*IntentNode, error) {
	// 容错处理：缓存为空或加载函数为空
	if c == nil || c.loadFn == nil {
		return []*IntentNode{}, nil
	}

	// 如果 Redis 未配置，直接从数据库加载
	if c.rdb == nil {
		return c.loadFn(ctx)
	}

	// 尝试从 Redis 获取缓存
	data, err := c.rdb.Get(ctx, intentTreeCacheKey).Bytes()
	if err == nil {
		var roots []*IntentNode
		if json.Unmarshal(data, &roots) == nil {
			slog.Debug("intent tree loaded from cache")
			return roots, nil // 缓存命中
		}
	}

	// 缓存未命中，从数据库加载
	roots, err := c.loadFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("load intent tree: %w", err)
	}

	// 写入 Redis 缓存（失败不影响主流程）
	if jsonData, err := json.Marshal(roots); err == nil {
		c.rdb.Set(ctx, intentTreeCacheKey, jsonData, intentTreeCacheTTL)
	}

	slog.Info("intent tree loaded from DB", "roots", len(roots))
	return roots, nil
}

// InvalidateCache 清除意图树缓存
// 调用时机：管理后台更新意图树后
func (c *TreeCache) InvalidateCache(ctx context.Context) {
	if c == nil || c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, intentTreeCacheKey)
}
