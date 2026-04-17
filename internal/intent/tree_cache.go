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
	intentTreeCacheKey = "ragent:intent:tree"
	intentTreeCacheTTL = 10 * time.Minute
)

// TreeCache provides Redis-cached access to the intent tree.
type TreeCache struct {
	rdb    *redis.Client
	loadFn func(ctx context.Context) ([]*IntentNode, error) // DB loader
}

var (
	defaultTreeCache   *TreeCache
	defaultTreeCacheMu sync.RWMutex
)

func NewTreeCache(rdb *redis.Client, loadFn func(ctx context.Context) ([]*IntentNode, error)) *TreeCache {
	c := &TreeCache{
		rdb:    rdb,
		loadFn: loadFn,
	}
	SetDefaultTreeCache(c)
	return c
}

func SetDefaultTreeCache(c *TreeCache) {
	defaultTreeCacheMu.Lock()
	defer defaultTreeCacheMu.Unlock()
	defaultTreeCache = c
}

func DefaultTreeCache() *TreeCache {
	defaultTreeCacheMu.Lock()
	defer defaultTreeCacheMu.Unlock()
	return defaultTreeCache
}

// GetRoots returns the intent tree roots, using Redis cache first.
func (c *TreeCache) GetRoots(ctx context.Context) ([]*IntentNode, error) {
	if c == nil || c.loadFn == nil {
		return []*IntentNode{}, nil
	}
	if c.rdb == nil {
		return c.loadFn(ctx)
	}
	data, err := c.rdb.Get(ctx, intentTreeCacheKey).Bytes()
	if err == nil {
		var roots []*IntentNode
		if json.Unmarshal(data, &roots) == nil {
			slog.Debug("intent tree loaded from cache")
			return roots, nil
		}
	}
	roots, err := c.loadFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("load intent tree: %w", err)
	}
	if jsonData, err := json.Marshal(roots); err != nil {
		c.rdb.Set(ctx, intentTreeCacheKey, jsonData, intentTreeCacheTTL)
	}

	slog.Info("intent tree loaded from DB", "roots", len(roots))
	return roots, nil
}

// InvalidateCache removes the cached intent tree.
func (c *TreeCache) InvalidateCache(ctx context.Context) {
	if c == nil || c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, intentTreeCacheKey)
}
