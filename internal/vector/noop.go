package vector

import (
	"context"
	"log/slog"
)

type NoOpService struct{}

func NewNoOpService() *NoOpService {
	return &NoOpService{}
}

// Search 返回空结果列表
// 模拟向量库无匹配结果的情况
func (s *NoOpService) Search(ctx context.Context, collection string, queryVec []float32, topK int) ([]SearchResult, error) {
	slog.Debug("noop vector search - returning empty results", "collection", collection)
	return []SearchResult{}, nil
}

// IndexChunks 跳过索引操作
func (s *NoOpService) IndexChunks(ctx context.Context, collection string, chunks []ChunkData) error {
	slog.Debug("noop vector index - skipping", "collection", collection, "count", len(chunks))
	return nil
}

// DeleteByDocID 跳过删除操作
func (s *NoOpService) DeleteByDocID(ctx context.Context, collection string, docID string) error {
	slog.Debug("noop vector delete - skipping", "collection", collection, "docID", docID)
	return nil
}

// UpdateChunk 跳过更新操作
func (s *NoOpService) UpdateChunk(ctx context.Context, collection string, chunk ChunkData) error {
	slog.Debug("noop vector update - skipping", "collection", collection, "chunkID", chunk.ID)
	return nil
}

// DeleteByChunkID 跳过删除操作
func (s *NoOpService) DeleteByChunkID(ctx context.Context, collection string, chunkID string) error {
	slog.Debug("noop vector delete chunk - skipping", "collection", collection, "chunkID", chunkID)
	return nil
}

// DeleteByChunkIDs 跳过批量删除操作
func (s *NoOpService) DeleteByChunkIDs(ctx context.Context, collection string, chunkIDs []string) error {
	slog.Debug("noop vector delete chunks - skipping", "collection", collection, "count", len(chunkIDs))
	return nil
}

// EnsureCollection 跳过集合创建
func (s *NoOpService) EnsureCollection(ctx context.Context, collection string, dimension int) error {
	slog.Debug("noop vector ensure collection - skipping", "collection", collection)
	return nil
}
