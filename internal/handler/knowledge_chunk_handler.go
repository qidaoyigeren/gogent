package handler

// knowledge_chunk_handler.go 提供对 KnowledgeChunkDO 的**手工运营管理**能力。
//
// 与 knowledge_doc_handler 的分工：
//   - knowledge_doc_handler：负责批量入库（pipeline/chunk 模式全量替换），是自动化流程的入口
//   - knowledge_chunk_handler：负责单块级运营（手工新增/修改/删除/启禁用），是管理端操作界面
//
// 核心设计原则：DB chunk 行和向量库条目始终同步（双写一致性）
//   - create：DB 写入后立即 embedding + IndexChunks（向量入库）
//   - update：DB 更新后如果 chunk 处于启用状态，重新 embedding + UpdateChunk（向量更新）
//   - delete：DB 删除 + DeleteByChunkID（向量删除）
//   - enable/batchEnable：
//       启用 → 重新 embedding + IndexChunks（确保向量存在）
//       禁用 → DeleteByChunkID（DB 记录保留，方便恢复；向量删除后检索不可见）
//
// 安全保护机制：
//   1. 文档 status == "running" 时，所有写操作均被拦截，避免手工编辑和入库任务并发修改
//   2. batchEnable 限制单次 500 个，避免大批量 embedding 请求超时或 OOM
//   3. 文档未启用时不允许新增/启用 chunk，防止操作无效数据

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"gogent/internal/auth"
	"gogent/internal/embedding"
	"gogent/internal/entity"
	"gogent/internal/token"
	"gogent/internal/vector"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type KnowledgeChunkHandler struct {
	db        *gorm.DB
	vectorSvc vector.VectorStoreService
	embSvc    embedding.EmbeddingService
}

// NewKnowledgeChunkHandler 创建 chunk 管理 Handler。
// 手工新增/修改/启禁用 chunk 时，需要同步 DB chunk 行和向量库。
func NewKnowledgeChunkHandler(db *gorm.DB, vectorSvc vector.VectorStoreService, embSvc embedding.EmbeddingService) *KnowledgeChunkHandler {
	return &KnowledgeChunkHandler{db: db, vectorSvc: vectorSvc, embSvc: embSvc}
}

// RegisterRoutes 注册文档 chunk 的查询、增删改和启用状态接口。
func (h *KnowledgeChunkHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/knowledge-base/docs/:docId/chunks", h.page)
	rg.POST("/knowledge-base/docs/:docId/chunks", h.create)
	rg.PUT("/knowledge-base/docs/:docId/chunks/:chunkId", h.update)
	rg.DELETE("/knowledge-base/docs/:docId/chunks/:chunkId", h.delete)
	rg.PATCH("/knowledge-base/docs/:docId/chunks/:chunkId/enable", h.enable)
	rg.PATCH("/knowledge-base/docs/:docId/chunks/batch-enable", h.batchEnable)
}

// page 分页查询某个文档的 chunk，可按 enabled 过滤。
func (h *KnowledgeChunkHandler) page(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	pageNo, pageSize := response.ParsePage(c)
	enabled := strings.TrimSpace(c.Query("enabled"))

	var chunks []entity.KnowledgeChunkDO
	var total int64
	q := h.db.Model(&entity.KnowledgeChunkDO{}).Where("doc_id = ?", docID)
	if enabled != "" {
		q = q.Where("enabled = ?", boolToInt(enabled == "true" || enabled == "1"))
	}
	q.Count(&total)
	q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("chunk_index ASC").Find(&chunks)
	response.SuccessPage(c, chunks, total, pageNo, pageSize)
}

// create 手工新增 chunk，并立即写入向量库。
//
// 业务流程：
//  1. 校验文档状态（不能 running、必须 enabled）
//  2. 校验内容非空，确定 chunkIndex（未指定则追加到最后）
//  3. DB 写入 chunk 行，更新文档 chunk_count +1
//  4. 同步写入向量库（embedding + IndexChunks）
//
// 并发安全：
//   - 文档处于 running 或被禁用时不允许新增，避免和入库任务并发修改同一批 chunk
//
// 双写一致性注意：
//   - 当前实现是“先写 DB 再写向量”，非事务双写
//   - 若向量写失败会返回错误，但 DB 行已存在；调用方可重试或后续用重建索引补偿
func (h *KnowledgeChunkHandler) create(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	var req struct {
		Content string `json:"content" binding:"required"`
		Index   *int   `json:"index"`
		ChunkID string `json:"chunkId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	doc, kb, err := h.loadDocumentAndKB(docID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块处理中，暂不支持新增 Chunk")
		return
	}
	if doc.Enabled != 1 {
		response.FailWithCode(c, errcode.ClientError, "文档未启用，暂不支持新增 Chunk")
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		response.FailWithCode(c, errcode.ClientError, "Chunk 内容不能为空")
		return
	}
	chunkIndex := 0
	if req.Index != nil {
		chunkIndex = *req.Index
	} else {
		// 未指定 index 时，查询当前文档最大的 chunkIndex 并 +1，实现自动追加到最后。
		var latest entity.KnowledgeChunkDO
		if err := h.db.Where("doc_id = ?", docID).Order("chunk_index DESC").First(&latest).Error; err == nil {
			chunkIndex = latest.ChunkIndex + 1
		}
	}

	chunk := entity.KnowledgeChunkDO{
		BaseModel:   entity.BaseModel{ID: firstNonBlank(req.ChunkID, idgen.NextIDStr())},
		DocID:       docID,
		KBID:        doc.KBID,
		ChunkIndex:  chunkIndex,
		Content:     content,
		ContentHash: chunkContentHash(content),
		CharCount:   len([]rune(content)),
		TokenCount:  countChunkTokens(content),
		Enabled:     entity.FlexEnabled(1),
		CreatedBy:   auth.GetUserID(c.Request.Context()),
		UpdatedBy:   auth.GetUserID(c.Request.Context()),
	}
	if err := h.db.Create(&chunk).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "新增Chunk失败")
		return
	}
	// 原子更新文档的 chunk_count，避免后续全量统计的开销。
	h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ?", docID).UpdateColumn("chunk_count", gorm.Expr("chunk_count + 1"))

	// 【关键】同步 chunk 到向量库：先写 DB 再写向量，非事务双写。
	// 若向量写失败会返回错误，但 DB 行已存在；调用方可重试或后续用重建索引补偿。
	if err := h.syncChunkToVector(c.Request.Context(), kb, doc, chunk); err != nil {
		// DB 已新增但向量同步失败时返回错误，提示用户重试或触发重建索引。
		response.FailWithCode(c, errcode.ServiceError, err.Error())
		return
	}
	response.Success(c, chunk)
}

// update 修改 chunk 内容，并在 chunk 启用时同步更新向量。
//
// 业务流程：
//  1. 校验文档状态（不能 running）
//  2. 加载 chunk 并校验归属（必须属于该文档）
//  3. 内容没变时直接返回，避免重复计算 hash/token 和重建向量（性能优化）
//  4. DB 更新 chunk 内容、hash、char_count、token_count
//  5. 仅启用状态的 chunk 才更新向量：禁用 chunk 在检索面应不可见，避免不必要写入
func (h *KnowledgeChunkHandler) update(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	chunkID := c.Param("chunkId")
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	doc, kb, err := h.loadDocumentAndKB(docID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块处理中，暂不支持修改 Chunk")
		return
	}
	var chunk entity.KnowledgeChunkDO
	if err := h.db.Where("id = ?", chunkID).First(&chunk).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不存在")
		return
	}
	if chunk.DocID != docID {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不属于该文档")
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		response.FailWithCode(c, errcode.ClientError, "Chunk 内容不能为空")
		return
	}
	if content == chunk.Content {
		// 内容没变时不重复计算 hash/token，也不重建向量，减少无效 embedding 调用。
		response.SuccessEmpty(c)
		return
	}
	chunk.Content = content
	chunk.ContentHash = chunkContentHash(content)
	chunk.CharCount = len([]rune(content))
	chunk.TokenCount = countChunkTokens(content)
	chunk.UpdatedBy = auth.GetUserID(c.Request.Context())
	if err := h.db.Model(&entity.KnowledgeChunkDO{}).Where("id = ?", chunkID).Updates(map[string]interface{}{
		"content":      content,
		"content_hash": chunk.ContentHash,
		"char_count":   chunk.CharCount,
		"token_count":  chunk.TokenCount,
		"updated_by":   chunk.UpdatedBy,
	}).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "更新Chunk失败")
		return
	}
	// 仅启用状态的 chunk 才更新向量：禁用 chunk 在检索面应不可见，避免不必要写入。
	// 这样设计可以节省 embedding 成本和向量库空间，同时保持检索结果清洁。
	if chunk.Enabled == 1 {
		if err := h.updateChunkVector(c.Request.Context(), kb, chunk); err != nil {
			response.FailWithCode(c, errcode.ServiceError, err.Error())
			return
		}
	}
	response.SuccessEmpty(c)
}

// delete 删除 chunk，并同步删除向量库中的同名 chunk。
//
// 业务流程：
//  1. 校验文档状态（不能 running）
//  2. 加载 chunk 并校验归属
//  3. DB 删除 chunk 行，更新文档 chunk_count -1
//  4. 删除向量库中的对应向量（确保检索不可见）
//
// 删除顺序：先删 DB 再删向量，保证管理端立刻不可见；
//
//	向量删失败会返回错误提示，用户可手动补偿或重建索引。
func (h *KnowledgeChunkHandler) delete(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	chunkID := c.Param("chunkId")
	doc, kb, err := h.loadDocumentAndKB(docID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块处理中，暂不支持删除 Chunk")
		return
	}
	var chunk entity.KnowledgeChunkDO
	if err := h.db.Where("id = ?", chunkID).First(&chunk).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不存在")
		return
	}
	if chunk.DocID != docID {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不属于该文档")
		return
	}
	if err := h.db.Where("id = ?", chunkID).Delete(&entity.KnowledgeChunkDO{}).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "删除Chunk失败")
		return
	}
	h.db.Model(&entity.KnowledgeDocumentDO{}).Where("id = ?", docID).
		UpdateColumn("chunk_count", gorm.Expr("CASE WHEN chunk_count > 0 THEN chunk_count - 1 ELSE 0 END"))

	// 先删 DB 再删向量，保证管理端立刻不可见；向量删失败会返回错误提示补偿。
	// 这个顺序确保即使向量删除失败，用户也看不到该 chunk（DB 已删）。
	if h.vectorSvc != nil && strings.TrimSpace(kb.CollectionName) != "" {
		if err := h.vectorSvc.DeleteByChunkID(c.Request.Context(), kb.CollectionName, chunkID); err != nil {
			response.FailWithCode(c, errcode.ServiceError, "删除Chunk向量失败")
			return
		}
	}
	response.SuccessEmpty(c)
}

// enable 启用或禁用单个 chunk。
//
// 语义：
//   - 启用：重新 embedding 并写向量（确保检索可见）
//   - 禁用：从向量库删除，DB 记录保留用于管理端恢复（检索不可见但可回溯）
//
// 向量操作遵循“启用=确保存在，禁用=确保删除”语义，保证检索面状态与 enabled 字段一致。
func (h *KnowledgeChunkHandler) enable(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	chunkID := c.Param("chunkId")
	value := c.Query("value") == "true"
	doc, kb, err := h.loadDocumentAndKB(docID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块处理中，暂不支持修改 Chunk 状态")
		return
	}
	if value && doc.Enabled != 1 {
		response.FailWithCode(c, errcode.ClientError, "文档未启用，暂不支持启用 Chunk")
		return
	}
	var chunk entity.KnowledgeChunkDO
	if err := h.db.Where("id = ?", chunkID).First(&chunk).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不存在")
		return
	}
	if chunk.DocID != docID {
		response.FailWithCode(c, errcode.ClientError, "Chunk 不属于该文档")
		return
	}
	if err := h.db.Model(&entity.KnowledgeChunkDO{}).Where("id = ?", chunkID).Updates(map[string]interface{}{
		"enabled":    boolToInt(value),
		"updated_by": auth.GetUserID(c.Request.Context()),
	}).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "更新Chunk状态失败")
		return
	}
	// 状态切换后的向量操作遵循“启用=确保存在，禁用=确保删除”语义。
	// 这个设计保证检索时只返回 enabled=1 的 chunk，且向量库状态与 DB 一致。
	if value {
		chunk.Enabled = 1
		if err := h.syncChunkToVector(c.Request.Context(), kb, doc, chunk); err != nil {
			response.FailWithCode(c, errcode.ServiceError, err.Error())
			return
		}
	} else if h.vectorSvc != nil && strings.TrimSpace(kb.CollectionName) != "" {
		if err := h.vectorSvc.DeleteByChunkID(c.Request.Context(), kb.CollectionName, chunkID); err != nil {
			response.FailWithCode(c, errcode.ServiceError, "禁用Chunk失败")
			return
		}
	}
	response.SuccessEmpty(c)
}

// batchEnable 批量启用/禁用 chunk，限制单次最多 500 个，避免一次请求压垮 embedding/向量库。
//
// 业务流程：
//  1. 校验文档状态（不能 running、必须 enabled 如果是启用操作）
//  2. 加载所有目标 chunk 并校验归属（必须全部属于该文档）
//  3. DB 批量更新 enabled 状态
//  4. 启用：逐条 syncChunkToVector（可优化为批量 embedding + 批量 IndexChunks）
//     禁用：批量 DeleteByChunkIDs（向量库支持批量删除）
//
// 性能考量：
//   - 限制 500 个是经验值，避免 embedding 请求超时或 OOM
//   - 启用时逐条 sync 是因为每条都需要独立 embedding，后续可优化为批量调用
func (h *KnowledgeChunkHandler) batchEnable(c *gin.Context) {
	docID, ok := docPathID(c)
	if !ok {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	value := c.Query("value") == "true"
	var req struct {
		IDs []string `json:"chunkIds"`
	}
	_ = c.ShouldBindJSON(&req)
	if len(req.IDs) == 0 {
		response.FailWithCode(c, errcode.ClientError, "请指定需要操作的 Chunk")
		return
	}
	if len(req.IDs) > 500 {
		response.FailWithCode(c, errcode.ClientError, "单次最多操作500个 Chunk")
		return
	}
	doc, kb, err := h.loadDocumentAndKB(docID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if strings.EqualFold(doc.Status, "running") {
		response.FailWithCode(c, errcode.ClientError, "文档正在分块处理中，暂不支持批量修改 Chunk 状态")
		return
	}
	if value && doc.Enabled != 1 {
		response.FailWithCode(c, errcode.ClientError, "文档未启用，暂不支持启用 Chunk")
		return
	}
	var chunks []entity.KnowledgeChunkDO
	if err := h.db.Where("doc_id = ? AND id IN ?", docID, req.IDs).Find(&chunks).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "查询Chunk失败")
		return
	}
	if len(chunks) != len(req.IDs) {
		response.FailWithCode(c, errcode.ClientError, "存在不属于该文档的 Chunk")
		return
	}
	if err := h.db.Model(&entity.KnowledgeChunkDO{}).Where("doc_id = ? AND id IN ?", docID, req.IDs).Updates(map[string]interface{}{
		"enabled":    boolToInt(value),
		"updated_by": auth.GetUserID(c.Request.Context()),
	}).Error; err != nil {
		response.FailWithCode(c, errcode.ServiceError, "批量更新Chunk状态失败")
		return
	}
	if value {
		// 逐条 sync 便于复用单条逻辑，但吞吐受限；后续可优化成批量 embedding + 批量 IndexChunks。
		// 当前实现：每条独立调用 embedding 模型，然后单独 IndexChunks。
		for _, chunk := range chunks {
			chunk.Enabled = 1
			if err := h.syncChunkToVector(c.Request.Context(), kb, doc, chunk); err != nil {
				response.FailWithCode(c, errcode.ServiceError, err.Error())
				return
			}
		}
	} else if h.vectorSvc != nil && strings.TrimSpace(kb.CollectionName) != "" {
		if err := h.vectorSvc.DeleteByChunkIDs(c.Request.Context(), kb.CollectionName, req.IDs); err != nil {
			response.FailWithCode(c, errcode.ServiceError, "批量禁用Chunk失败")
			return
		}
	}
	response.SuccessEmpty(c)
}

// loadDocumentAndKB 同时加载文档和所属知识库，作为 chunk 操作的基础校验。
func (h *KnowledgeChunkHandler) loadDocumentAndKB(docID string) (entity.KnowledgeDocumentDO, entity.KnowledgeBaseDO, error) {
	var doc entity.KnowledgeDocumentDO
	if err := h.db.Where("id = ? AND deleted = 0", docID).First(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return entity.KnowledgeDocumentDO{}, entity.KnowledgeBaseDO{}, errcode.NewClientError("文档不存在")
		}
		slog.Error("chunk handler load document failed", "docId", docID, "err", err)
		return entity.KnowledgeDocumentDO{}, entity.KnowledgeBaseDO{}, errcode.NewServiceError("查询文档失败")
	}
	var kb entity.KnowledgeBaseDO
	if err := h.db.Where("id = ? AND deleted = 0", doc.KBID).First(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return entity.KnowledgeDocumentDO{}, entity.KnowledgeBaseDO{}, errcode.NewClientError("知识库不存在")
		}
		slog.Error("chunk handler load knowledge base failed", "kbId", doc.KBID, "docId", docID, "err", err)
		return entity.KnowledgeDocumentDO{}, entity.KnowledgeBaseDO{}, errcode.NewServiceError("查询知识库失败")
	}
	return doc, kb, nil
}

// syncChunkToVector 将单个 chunk embedding 后写入向量库。
// 用于新增 chunk、重新启用 chunk 等“向量不存在或需要覆盖写入”的场景。
func (h *KnowledgeChunkHandler) syncChunkToVector(ctx context.Context, kb entity.KnowledgeBaseDO, doc entity.KnowledgeDocumentDO, chunk entity.KnowledgeChunkDO) error {
	if h.vectorSvc == nil || strings.TrimSpace(kb.CollectionName) == "" {
		// 允许“仅数据库模式”，常见于本地开发或向量服务降级。
		return nil
	}
	vectorValue, err := h.embedChunk(ctx, kb.EmbeddingModel, chunk.Content)
	if err != nil {
		return err
	}
	return h.vectorSvc.IndexChunks(ctx, kb.CollectionName, []vector.ChunkData{{
		ID:      chunk.ID,
		DocID:   doc.ID,
		Index:   chunk.ChunkIndex,
		Content: chunk.Content,
		Vector:  vectorValue,
		Metadata: map[string]string{
			"kbId":  doc.KBID,
			"docId": doc.ID,
		},
	}})
}

// updateChunkVector 重新生成向量并更新已存在的 chunk 向量。
func (h *KnowledgeChunkHandler) updateChunkVector(ctx context.Context, kb entity.KnowledgeBaseDO, chunk entity.KnowledgeChunkDO) error {
	if h.vectorSvc == nil || strings.TrimSpace(kb.CollectionName) == "" {
		return nil
	}
	vectorValue, err := h.embedChunk(ctx, kb.EmbeddingModel, chunk.Content)
	if err != nil {
		return err
	}
	return h.vectorSvc.UpdateChunk(ctx, kb.CollectionName, vector.ChunkData{
		ID:      chunk.ID,
		DocID:   chunk.DocID,
		Index:   chunk.ChunkIndex,
		Content: chunk.Content,
		Vector:  vectorValue,
	})
}

// embedChunk 按知识库配置选择 embedding 模型；未配置模型时使用默认 embedding 服务。
func (h *KnowledgeChunkHandler) embedChunk(ctx context.Context, modelID, content string) ([]float32, error) {
	if h.embSvc == nil {
		return nil, nil
	}
	if svc, ok := h.embSvc.(embedding.ModelSelectableEmbeddingService); ok && strings.TrimSpace(modelID) != "" {
		return svc.EmbedSingleWithModelID(ctx, modelID, content)
	}
	return h.embSvc.EmbedSingle(ctx, content)
}

// firstNonBlank 返回第一个非空字符串，用于允许调用方自定义 chunkID。
func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// countChunkTokens 使用启发式 token 计数器估算 chunk 长度。
func countChunkTokens(content string) int {
	return token.NewHeuristicCounter().Count(content)
}

// chunkContentHash 计算 chunk 内容哈希，便于检测内容是否变化。
func chunkContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
