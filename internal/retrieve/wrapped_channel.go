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

// IsEnabled implements [SearchChannel].
func (w *wrappedChannel) IsEnabled(ctx context.Context, reqCtx *RetrievalContext) bool {
	if !w.enabled {
		return false
	}
	return w.inner.IsEnabled(ctx, reqCtx)
}

// Name implements [SearchChannel].
func (w *wrappedChannel) Name() string {
	return w.inner.Name()
}

// Priority implements [SearchChannel].
func (w *wrappedChannel) Priority() int {
	return w.inner.Priority()
}

// Search implements [SearchChannel].
func (w *wrappedChannel) Search(ctx context.Context, reqCtx *RetrievalContext) ([]DocumentChunk, error) {
	return w.inner.Search(ctx, reqCtx)
}

func NewWrappedChannel(inner SearchChannel, priority int, enabled bool) SearchChannel {
	return &wrappedChannel{inner: inner, priority: priority, enabled: enabled}
}
