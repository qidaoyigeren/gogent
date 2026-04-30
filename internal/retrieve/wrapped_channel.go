package retrieve

import (
	"context"
)

// wrappedChannel 包装通道
type wrappedChannel struct {
	inner    SearchChannel // 原始通道（如 IntentDirectedChannel）
	priority int           // 配置级优先级（覆盖 inner 的 Priority）
	enabled  bool          // 配置级启用标志
}

// NewWrappedChannel 创建包装通道
// 参数：
//   - inner: 原始通道
//   - priority: 配置级优先级（数字越小优先级越高）
//   - enabled: 配置级启用标志（false 则直接禁用）
//
// 示例：
//
//	ch := NewWrappedChannel(
//	    NewIntentDirectedChannel(...),
//	    1,     // priority
//	    true,  // enabled
//	)
func NewWrappedChannel(inner SearchChannel, priority int, enabled bool) SearchChannel {
	return &wrappedChannel{inner: inner, priority: priority, enabled: enabled}
}

// Name 返回通道名称（代理到 inner）
func (w *wrappedChannel) Name() string { return w.inner.Name() }

// Priority 返回配置级优先级（不使用 inner 的优先级）
func (w *wrappedChannel) Priority() int { return w.priority }

// IsEnabled 判断通道是否启用
// 检查逻辑：
// 1. 先检查配置级 enabled（如果为 false，直接返回 false）
// 2. 再调用 inner 的 IsEnabled（业务级判断）
// 3. 两者都为 true 才启用
func (w *wrappedChannel) IsEnabled(ctx context.Context, reqCtx *RetrievalContext) bool {
	// 配置级禁用 → 直接返回 false
	if !w.enabled {
		return false
	}
	// 配置级启用 → 继续检查业务级
	return w.inner.IsEnabled(ctx, reqCtx)
}

// Search 执行检索（代理到 inner）
func (w *wrappedChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	return w.inner.Search(ctx, reqCtx)
}
