package vector

import "context"

// SearchResult 表示向量搜索的单个结果
// 从向量库检索后返回给调用方的数据结构
type SearchResult struct {
	ID       string            // Chunk ID（文档块的唯一标识）
	Content  string            // Chunk 内容（文档块的文本）
	Score    float64           // 相似度分数（越高越相关）
	Metadata map[string]string // 元数据（doc_id、kb_id 等）
}

// VectorStoreService 向量存储服务接口
// 核心职责：
// 1. 向量索引：将文本向量存储到向量数据库
// 2. 相似度搜索：根据查询向量检索最相关的文档
// 3. 数据管理：删除、更新文档块
type VectorStoreService interface {
	// Search 执行相似度搜索
	Search(ctx context.Context, collection string, queryVec []float32, topK int) ([]SearchResult, error)

	// IndexChunks 批量索引文档块
	// 用于文档入库时，将所有 chunk 的向量写入向量库
	IndexChunks(ctx context.Context, collection string, chunks []ChunkData) error

	// UpdateChunk 更新单个文档块
	// 使用 UPSERT 语义（存在则更新，不存在则插入）
	UpdateChunk(ctx context.Context, collection string, chunk ChunkData) error

	// DeleteByDocID 删除文档的所有块
	// 用于文档删除或重新索引时
	DeleteByDocID(ctx context.Context, collection string, docID string) error

	// DeleteByChunkID 删除单个文档块
	DeleteByChunkID(ctx context.Context, collection string, chunkID string) error

	// DeleteByChunkIDs 批量删除文档块
	DeleteByChunkIDs(ctx context.Context, collection string, chunkIDs []string) error

	// EnsureCollection 确保集合存在
	// 如果集合不存在则创建（包括索引）
	// 幂等操作，可安全重复调用
	EnsureCollection(ctx context.Context, collection string, dimension int) error
}

// ChunkData 表示待索引的文档块
// 包含文本内容、向量和元数据
type ChunkData struct {
	ID       string            // Chunk ID（唯一标识）
	DocID    string            // 文档 ID（所属文档）
	Index    int               // Chunk 索引（在文档中的顺序）
	Content  string            // Chunk 内容（文本）
	Vector   []float32         // 向量数据（由 embedding 模型生成）
	Metadata map[string]string // 元数据（key-value 对）
}
