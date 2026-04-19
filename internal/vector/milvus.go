package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const (
	fieldID        = "id"
	fieldContent   = "content"
	fieldDocID     = "doc_id"
	fieldMetadata  = "metadata"
	fieldEmbedding = "embedding"
)

// MilvusService implements VectorStoreService for Milvus.
type MilvusService struct {
	uri    string
	cli    client.Client
	inited bool
}

func NewMilvusService(uri string) *MilvusService {
	return &MilvusService{uri: uri}
}

func (s *MilvusService) ensureClient(ctx context.Context) error {
	if s.inited {
		return nil
	}
	cli, err := client.NewClient(ctx, client.Config{
		Address: s.uri,
	})
	if err != nil {
		return fmt.Errorf("milvus connect: %w", err)
	}
	s.cli = cli
	s.inited = true
	return nil
}

func (s *MilvusService) Search(ctx context.Context, collection string, queryVec []float32, topK int) ([]SearchResult, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}

	slog.Debug("milvus search", "collection", collection, "topK", topK, "vecDim", len(queryVec))

	// Load collection if not loaded
	_ = s.cli.LoadCollection(ctx, collection, false)

	sp, _ := entity.NewIndexIvfFlatSearchParam(64)
	results, err := s.cli.Search(
		ctx, collection, nil, "",
		[]string{fieldID, fieldContent, fieldDocID, fieldMetadata},
		[]entity.Vector{entity.FloatVector(queryVec)},
		fieldEmbedding,
		entity.COSINE,
		topK,
		sp,
	)
	if err != nil {
		return nil, fmt.Errorf("milvus search: %w", err)
	}

	var out []SearchResult
	for _, sr := range results {
		for i := 0; i < sr.ResultCount; i++ {
			var id, content, docID, metaStr string
			for _, field := range sr.Fields {
				switch field.Name() {
				case fieldID:
					if col, ok := field.(*entity.ColumnVarChar); ok {
						id, _ = col.ValueByIdx(i)
					}
				case fieldContent:
					if col, ok := field.(*entity.ColumnVarChar); ok {
						content, _ = col.ValueByIdx(i)
					}
				case fieldDocID:
					if col, ok := field.(*entity.ColumnVarChar); ok {
						docID, _ = col.ValueByIdx(i)
					}
				case fieldMetadata:
					if col, ok := field.(*entity.ColumnVarChar); ok {
						metaStr, _ = col.ValueByIdx(i)
					}
				}
			}

			meta := map[string]string{"doc_id": docID}
			if metaStr != "" {
				_ = json.Unmarshal([]byte(metaStr), &meta)
			}

			score := float64(0)
			if i < len(sr.Scores) {
				score = float64(sr.Scores[i])
			}

			out = append(out, SearchResult{
				ID:       id,
				Content:  content,
				Score:    score,
				Metadata: meta,
			})
		}
	}

	return out, nil
}

func (s *MilvusService) IndexChunks(ctx context.Context, collection string, chunks []ChunkData) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	slog.Info("milvus index", "collection", collection, "count", len(chunks))

	ids := make([]string, len(chunks))
	contents := make([]string, len(chunks))
	docIDs := make([]string, len(chunks))
	metadatas := make([]string, len(chunks))
	vectors := make([][]float32, len(chunks))

	for i, c := range chunks {
		ids[i] = c.ID
		// Truncate content if too long for VarChar
		content := c.Content
		if len(content) > 65000 {
			content = content[:65000]
		}
		contents[i] = content
		docIDs[i] = c.DocID
		metaBytes, _ := json.Marshal(c.Metadata)
		metadatas[i] = string(metaBytes)
		vectors[i] = c.Vector
	}

	_, err := s.cli.Insert(
		ctx, collection, "",
		entity.NewColumnVarChar(fieldID, ids),
		entity.NewColumnVarChar(fieldContent, contents),
		entity.NewColumnVarChar(fieldDocID, docIDs),
		entity.NewColumnVarChar(fieldMetadata, metadatas),
		entity.NewColumnFloatVector(fieldEmbedding, len(chunks[0].Vector), vectors),
	)
	if err != nil {
		return fmt.Errorf("milvus insert: %w", err)
	}

	// Flush to ensure data is persisted
	_ = s.cli.Flush(ctx, collection, false)

	return nil
}

func (s *MilvusService) DeleteByDocID(ctx context.Context, collection string, docID string) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	slog.Info("milvus delete", "collection", collection, "docID", docID)
	expr := fmt.Sprintf(`metadata["doc_id"] == "%s"`, docID)
	return s.cli.Delete(ctx, collection, "", expr)
}

func (s *MilvusService) UpdateChunk(ctx context.Context, collection string, chunk ChunkData) error {
	// Milvus uses upsert for update
	return s.IndexChunks(ctx, collection, []ChunkData{chunk})
}

func (s *MilvusService) DeleteByChunkID(ctx context.Context, collection string, chunkID string) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	slog.Info("milvus delete chunk", "collection", collection, "chunkID", chunkID)
	expr := fmt.Sprintf(`id == "%s"`, chunkID)
	return s.cli.Delete(ctx, collection, "", expr)
}

func (s *MilvusService) DeleteByChunkIDs(ctx context.Context, collection string, chunkIDs []string) error {
	if len(chunkIDs) == 0 {
		return nil
	}
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	slog.Info("milvus delete chunks", "collection", collection, "count", len(chunkIDs))
	idList := make([]string, len(chunkIDs))
	for i, id := range chunkIDs {
		idList[i] = fmt.Sprintf(`"%s"`, id)
	}
	expr := fmt.Sprintf(`id in [%s]`, strings.Join(idList, ", "))
	return s.cli.Delete(ctx, collection, "", expr)
}

func (s *MilvusService) EnsureCollection(ctx context.Context, collection string, dimension int) error {
	if err := s.ensureClient(ctx); err != nil {
		return err
	}

	exists, err := s.cli.HasCollection(ctx, collection)
	if err != nil {
		return fmt.Errorf("milvus has collection: %w", err)
	}
	if exists {
		return nil
	}

	slog.Info("creating milvus collection", "collection", collection, "dimension", dimension)

	schema := &entity.Schema{
		CollectionName: collection,
		AutoID:         false,
		Fields: []*entity.Field{
			{Name: fieldID, DataType: entity.FieldTypeVarChar, PrimaryKey: true, TypeParams: map[string]string{"max_length": "128"}},
			{Name: fieldContent, DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "65535"}},
			{Name: fieldDocID, DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "128"}},
			{Name: fieldMetadata, DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "4096"}},
			{Name: fieldEmbedding, DataType: entity.FieldTypeFloatVector, TypeParams: map[string]string{"dim": fmt.Sprintf("%d", dimension)}},
		},
	}

	if err := s.cli.CreateCollection(ctx, schema, 1); err != nil {
		return fmt.Errorf("milvus create collection: %w", err)
	}

	// Create IVF_FLAT index on embedding field
	idx, _ := entity.NewIndexIvfFlat(entity.COSINE, 128)
	if err := s.cli.CreateIndex(ctx, collection, fieldEmbedding, idx, false); err != nil {
		slog.Warn("milvus create index failed", "err", err)
	}

	return nil
}
