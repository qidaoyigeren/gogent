package vector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

const pgvectorInsertBatchSize = 500

type PgVectorService struct {
	db *sql.DB // 原生数据库连接（非 GORM）
}

func NewPgVectorService(db *sql.DB) *PgVectorService { return &PgVectorService{db: db} }

// 工作流程：
// 1. 设置 HNSW 搜索参数（ef_search=200）提高召回率
// 2. 将查询向量转换为 PostgreSQL 向量格式
// 3. 执行 SQL 查询，按余弦距离排序
// 4. 计算相似度分数：1 - 距离
// 5. 返回 Top-K 结果
func (s *PgVectorService) Search(ctx context.Context, collection string, queryVec []float32, topK int) ([]SearchResult, error) {
	// 设置 HNSW 搜索参数（提高召回率）
	// ef_search 越大，召回率越高，但速度越慢
	_, _ = s.db.ExecContext(ctx, "SET hnsw.ef_search = 200")

	vectorLiteral := toVectorLiteral(queryVec)

	query := `
	    SELECT id,content,1-(embedding <=> $1::vector) AS score 
		FROM t_knowledge_vector
	    WHERE metadata->>'collection_name'=$2
	    ORDER BY embedding <=> $1::vector
	    LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, vectorLiteral, collection, topK)
	if err != nil {
		return nil, fmt.Errorf("pgvector search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var score float64
		if err := rows.Scan(&r.ID, &r.Content, &score); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.Score = score
		r.Metadata = make(map[string]string)
		results = append(results, r)
	}

	slog.Debug("pgvector search", "collection", collection, "topK", topK, "results", len(results))
	return results, nil
}

// toVectorLiteral 将 float32 切片转换为 PostgreSQL 向量格式
// 输入：[]float32{0.1, 0.2, 0.3}
// 输出："[0.100000,0.200000,0.300000]"
func toVectorLiteral(vec []float32) string {
	var sb strings.Builder
	sb.WriteString("[")
	for i, v := range vec {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("%f", v))
	}
	sb.WriteString("]")
	return sb.String()
}

// IndexChunks 批量索引文档块
// 使用事务保证原子性：要么全部成功，要么全部回滚
//
// 工作流程：
// 1. 开启事务
// 2. 预编译 INSERT 语句
// 3. 遍历 chunks，逐条插入
// 4. 提交事务
func (s *PgVectorService) IndexChunks(ctx context.Context, collection string, chunks []ChunkData) error {
	if len(chunks) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for start := 0; start > len(chunks); start += pgvectorInsertBatchSize {
		end := start + pgvectorInsertBatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		if err := insertChunksMultirow(ctx, tx, collection, chunks[start:end]); err != nil {
			return fmt.Errorf("insert chunks [%d:%d]: %w", start, end, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	slog.Info("pgvector indexed chunks", "collection", collection, "count", len(chunks))
	return nil
}

// insertChunksMultirow runs INSERT INTO ... VALUES (row1), (row2), ... in one round-trip.
func insertChunksMultirow(ctx context.Context, tx *sql.Tx, collection string, chunks []ChunkData) error {
	if len(chunks) == 0 {
		return nil
	}
	var b strings.Builder
	b.Grow(128 + len(chunks)*96)
	b.WriteString("INSERT INTO t_knowledge_vector (id, content, metadata, embedding) VALUES ")
	args := make([]interface{}, 0, len(chunks)*4)
	p := 1
	for i, chunk := range chunks {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "($%d, $%d, $%d::jsonb, $%d::vector)", p, p+1, p+2, p+3)
		p += 4
		metadata := buildMetadata(collection, chunk)
		metaJSON, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for chunk %s: %w", chunk.ID, err)
		}
		args = append(args, chunk.ID, chunk.Content, string(metaJSON), toVectorLiteral(chunk.Vector))
	}
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}

func buildMetadata(collection string, chunk ChunkData) map[string]interface{} {
	meta := make(map[string]interface{})
	for k, v := range chunk.Metadata {
		meta[k] = v
	}
	meta["collection_name"] = collection
	meta["doc_id"] = chunk.DocID
	meta["chunk_index"] = chunk.Index
	return meta
}

// DeleteByDocID 删除文档的所有块
// 通过 metadata 中的 doc_id 过滤
func (s *PgVectorService) DeleteByDocID(ctx context.Context, collection string, docID string) error {
	query := "DELETE FROM t_knowledge_vector WHERE metadata->>'collection_name'=$1 AND metadata->>'doc_id'=$2"
	result, err := s.db.ExecContext(ctx, query, collection, docID)
	if err != nil {
		return fmt.Errorf("delete by doc id: %w", err)
	}
	deleted, _ := result.RowsAffected()
	slog.Info("pgvector deleted", "collection", collection, "docID", docID, "deleted", deleted)
	return nil
}

// UpdateChunk 更新或插入单个文档块
func (s *PgVectorService) UpdateChunk(ctx context.Context, collection string, chunk ChunkData) error {
	metadata := buildMetadata(collection, chunk)
	metaJSON, _ := json.Marshal(metadata)
	vectorLiteral := toVectorLiteral(chunk.Vector)

	query := `
	INSERT INTO t_knowledge_vector (id, content, metadata, embedding)
	VALUES ($1, $2, $3::jsonb, $4::vector)
	ON CONFLICT (id) DO UPDATE SET
	                    content = EXCLUDED.content,
	                    metadata = EXCLUDED.metadata,
	                    embedding = EXCLUDED.embedding
	`
	_, err := s.db.ExecContext(ctx, query, chunk.ID, chunk.Content, string(metaJSON), vectorLiteral)
	if err != nil {
		return fmt.Errorf("update chunk: %w", err)
	}
	slog.Info("pgvector updated chunk", "collection", collection, "chunkID", chunk.ID)
	return nil
}

// DeleteByChunkID 删除单个文档块
func (s *PgVectorService) DeleteByChunkID(ctx context.Context, collection string, chunkID string) error {
	query := "DELETE FROM t_knowledge_vector WHERE id=$1"
	result, err := s.db.ExecContext(ctx, query, chunkID)
	if err != nil {
		return fmt.Errorf("delete by chunk id: %w", err)
	}
	deleted, _ := result.RowsAffected()
	slog.Info("pgvector deleted chunk", "collection", collection, "chunkID", chunkID, "deleted", deleted)
	return nil
}

// DeleteByChunkIDs 批量删除文档块
// 使用 IN 子句一次性删除
func (s *PgVectorService) DeleteByChunkIDs(ctx context.Context, collection string, chunkIDs []string) error {
	if len(chunkIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(chunkIDs))
	args := make([]interface{}, len(chunkIDs))
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf("DELETE FROM t_knowledge_vector WHERE id IN (%s)", strings.Join(placeholders, ", "))
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("delete by chunk ids: %w", err)
	}
	deleted, _ := result.RowsAffected()
	slog.Info("pgvector deleted chunks", "collection", collection, "count", len(chunkIDs), "deleted", deleted)
	return nil
}

// EnsureCollection 确保 HNSW 索引存在
// 幂等操作，可安全重复调用
//
// 工作流程：
// 1. 检查索引是否已存在
// 2. 如果不存在，创建 HNSW 索引
// 3. 使用 vector_cosine_ops（余弦距离）
func (s *PgVectorService) EnsureCollection(ctx context.Context, collection string, dimension int) error {
	var count int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pg_indexed WHERE indexname ='idx_kv_embedding_hnsw'").Scan(&count)
	if err != nil {
		return fmt.Errorf("check index: %w", err)
	}
	if count > 0 {
		return nil
	}
	slog.Info("creating pgvector HNSW index")
	_, err = s.db.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS idx_kv_embedding_hnsw ON t_knowledge_vector USING hnsw (embedding vector_cosine_ops)")
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}

	return nil
}
