package handler

// knowledge_base_handler.go 管理知识库元数据与生命周期。

import (
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/internal/storage"
	"gogent/internal/vector"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type KnowledgeBaseHandler struct {
	db          *gorm.DB
	vectorSvc   vector.VectorStoreService
	dimension   int
	bucketAdmin storage.BucketAdmin // optional; non-nil only when S3 storage is active
}

// NewKnowledgeBaseHandler 创建知识库管理 Handler。
// vectorSvc/dimension 用于创建知识库时预创建向量集合。
func NewKnowledgeBaseHandler(db *gorm.DB, vectorSvc vector.VectorStoreService, dimension int) *KnowledgeBaseHandler {
	return &KnowledgeBaseHandler{db: db, vectorSvc: vectorSvc, dimension: dimension}
}

// WithBucketAdmin attaches an S3 BucketAdmin so that create() auto-provisions
// a per-KB bucket using the KB's collectionName.
func (h *KnowledgeBaseHandler) WithBucketAdmin(ba storage.BucketAdmin) *KnowledgeBaseHandler {
	h.bucketAdmin = ba
	return h
}

// RegisterRoutes 注册知识库元数据管理和分块策略查询接口。
func (h *KnowledgeBaseHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/knowledge-base", h.create)
	rg.PUT("/knowledge-base/:kbId", h.update)
	rg.DELETE("/knowledge-base/:kbId", h.delete)
	rg.GET("/knowledge-base/:kbId", h.get)
	rg.GET("/knowledge-base", h.page)
	rg.GET("/knowledge-base/chunk-strategies", h.chunkStrategies)
}

// create 创建知识库，同时准备对象存储 bucket 和向量 collection。
func (h *KnowledgeBaseHandler) create(c *gin.Context) {
	var req struct {
		Name           string `json:"name" binding:"required"`
		EmbeddingModel string `json:"embeddingModel"`
		CollectionName string `json:"collectionName"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	// Validate: name must be unique
	name := strings.TrimSpace(req.Name)
	var count int64
	h.db.Model(&entity.KnowledgeBaseDO{}).Where("name = ? AND deleted = 0", name).Count(&count)
	if count > 0 {
		response.FailWithCode(c, errcode.ClientError, "知识库名称已存在："+name)
		return
	}

	id := idgen.NextIDStr()
	collName := req.CollectionName
	if collName == "" {
		// collectionName 是向量库命名空间，未传时用知识库 ID 派生一个稳定名称。
		collName = "kb_" + id[:8]
	}
	kb := entity.KnowledgeBaseDO{
		BaseModel:      entity.BaseModel{ID: id},
		Name:           name,
		EmbeddingModel: req.EmbeddingModel,
		CollectionName: collName,
		CreatedBy:      auth.GetUserID(c.Request.Context()),
	}
	h.db.Create(&kb)
	// 这里未显式检查 Create 错误是历史实现；后续 bucket/vector 失败时会返回错误，
	// 但可能已经有 DB 记录。若需强一致，应改为事务 + 补偿删除。

	// Provision per-KB S3 bucket when running with object storage.
	if h.bucketAdmin != nil {
		// bucket 名沿用 collectionName，保持对象存储与向量空间一一对应，便于运维排查。
		if err := h.bucketAdmin.EnsureBucket(c.Request.Context(), collName); err != nil {
			slog.Error("failed to create KB S3 bucket", "bucket", collName, "err", err)
			response.FailWithCode(c, errcode.ServiceError, err.Error())
			return
		}
	}

	// Ensure vector collection exists
	if h.vectorSvc != nil {
		dim := h.dimension
		if dim <= 0 {
			dim = 1536 // safe fallback, but prefer config injection
		}
		// 忽略 Ensure 错误是“尽量创建成功”的折中；真实写向量失败会在入库时暴露。
		_ = h.vectorSvc.EnsureCollection(c.Request.Context(), collName, dim)
	}

	response.Success(c, id)
}

// update 更新知识库名称和 embedding 模型。
// 已存在向量化文档时禁止修改 embedding 模型，避免旧向量维度/语义空间和新模型不一致。
func (h *KnowledgeBaseHandler) update(c *gin.Context) {
	kbID := c.Param("kbId")

	var kb entity.KnowledgeBaseDO
	if err := h.db.Where("id = ? AND deleted = 0", kbID).First(&kb).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "知识库不存在")
		return
	}

	var req struct {
		Name           string `json:"name"`
		EmbeddingModel string `json:"embeddingModel"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	updates := map[string]interface{}{}

	// Validate name when provided: must not be blank after trimming.
	if req.Name != "" {
		name := strings.TrimSpace(req.Name)
		if name == "" {
			response.FailWithCode(c, errcode.ClientError, "知识库名称不能为空")
			return
		}
		var count int64
		h.db.Model(&entity.KnowledgeBaseDO{}).Where("name = ? AND id != ? AND deleted = 0", name, kbID).Count(&count)
		if count > 0 {
			response.FailWithCode(c, errcode.ClientError, "知识库名称已存在："+name)
			return
		}
		updates["name"] = name
	}

	// Validate embedding model change: block if docs are already vectorized
	if req.EmbeddingModel != "" && req.EmbeddingModel != kb.EmbeddingModel {
		// 核心约束：已有 chunk 时改 embeddingModel 会造成新旧向量语义空间不一致。
		var docCount int64
		h.db.Model(&entity.KnowledgeDocumentDO{}).Where("kb_id = ? AND chunk_count > 0 AND deleted = 0", kbID).Count(&docCount)
		if docCount > 0 {
			response.FailWithCode(c, errcode.ClientError, "知识库已存在向量化文档，不允许修改嵌入模型")
			return
		}
		updates["embedding_model"] = req.EmbeddingModel
	}

	if len(updates) > 0 {
		updates["updated_by"] = auth.GetUserID(c.Request.Context())
		h.db.Model(&entity.KnowledgeBaseDO{}).Where("id = ?", kbID).Updates(updates)
	}
	response.SuccessEmpty(c)
}

// delete 删除空知识库；有文档时阻止删除，避免遗留 chunk 和向量数据。
func (h *KnowledgeBaseHandler) delete(c *gin.Context) {
	kbID := c.Param("kbId")

	// Check KB exists
	var kb entity.KnowledgeBaseDO
	if err := h.db.Where("id = ? AND deleted = 0", kbID).First(&kb).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "知识库不存在")
		return
	}

	// Check if KB has documents
	// 这里以文档数做删除前置，而非 chunk 数：避免“文档还在但 chunk 被清空”时误删 KB。
	var docCount int64
	h.db.Model(&entity.KnowledgeDocumentDO{}).Where("kb_id = ? AND deleted = 0", kbID).Count(&docCount)
	if docCount > 0 {
		response.FailWithCode(c, errcode.ClientError, "当前知识库下还有文档，请先删除文档")
		return
	}

	h.db.Model(&entity.KnowledgeBaseDO{}).Where("id = ?", kbID).Update("deleted", 1)
	response.SuccessEmpty(c)
}

// get 查询单个知识库元数据。
func (h *KnowledgeBaseHandler) get(c *gin.Context) {
	kbID := c.Param("kbId")
	var kb entity.KnowledgeBaseDO
	if err := h.db.Where("id = ? AND deleted = 0", kbID).First(&kb).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "知识库不存在")
		return
	}
	response.Success(c, kb)
}

// page 分页查询知识库，并聚合每个知识库下的文档数量。
func (h *KnowledgeBaseHandler) page(c *gin.Context) {
	pageNo, pageSize := response.ParsePage(c)
	name := c.Query("name") // Java uses "name" parameter, not "keyword"

	var list []entity.KnowledgeBaseDO
	var total int64
	q := h.db.Model(&entity.KnowledgeBaseDO{}).Where("deleted = 0")
	if name != "" {
		q = q.Where("name LIKE ?", "%"+name+"%")
	}
	q.Count(&total)
	q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("update_time DESC").Find(&list)

	// Aggregate documentCount per KB (matching Java pageQuery behavior)
	type KnowledgeBaseVO struct {
		entity.KnowledgeBaseDO
		DocumentCount int64 `json:"documentCount"`
	}
	kbIDs := make([]string, 0, len(list))
	for _, kb := range list {
		kbIDs = append(kbIDs, kb.ID)
	}
	docCountMap := make(map[string]int64)
	if len(kbIDs) > 0 {
		type docCountRow struct {
			KBID string `gorm:"column:kb_id"`
			Cnt  int64  `gorm:"column:cnt"`
		}
		var rows []docCountRow
		h.db.Model(&entity.KnowledgeDocumentDO{}).
			Select("kb_id, count(1) as cnt").
			Where("kb_id IN ? AND deleted = 0", kbIDs).
			Group("kb_id").
			Find(&rows)
		for _, r := range rows {
			docCountMap[r.KBID] = r.Cnt
		}
	}

	vos := make([]KnowledgeBaseVO, 0, len(list))
	for _, kb := range list {
		vos = append(vos, KnowledgeBaseVO{
			KnowledgeBaseDO: kb,
			DocumentCount:   docCountMap[kb.ID],
		})
	}
	response.SuccessPage(c, vos, total, pageNo, pageSize)
}

// chunkStrategies 返回前端创建文档时可选的分块策略及默认配置。
func (h *KnowledgeBaseHandler) chunkStrategies(c *gin.Context) {
	strategies := []gin.H{
		{"value": "fixed_size", "label": "固定大小分块", "defaultConfig": gin.H{"chunkSize": 512, "overlapSize": 128}},
		{"value": "structure_aware", "label": "结构感知分块", "defaultConfig": gin.H{"targetChars": 1400, "overlapChars": 0, "maxChars": 1800, "minChars": 600}},
	}
	response.Success(c, strategies)
}
