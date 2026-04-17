package handler

import (
	"encoding/json"
	"fmt"
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/internal/intent"
	"gogent/internal/rewrite"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// flexBoolInt unmarshals a JSON field that may be boolean (true/false) or
// integer (0/1) and normalises it to a Go int pointer.
// This bridges the frontend (boolean) ↔ DB (int) contract for "enabled" fields.
type flexBoolInt struct {
	set bool
	val int
}

func (f *flexBoolInt) UnmarshalJSON(b []byte) error {
	f.set = true
	s := strings.TrimSpace(string(b))
	switch s {
	case "true":
		f.val = 1
	case "false":
		f.val = 0
	case "null":
		f.set = false
	default:
		var n int
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		f.val = n
	}
	return nil
}

func (f *flexBoolInt) Ptr() *int {
	if !f.set {
		return nil
	}
	v := f.val
	return &v
}

type IntentTreeHandler struct{ db *gorm.DB }

func NewIntentTreeHandler(db *gorm.DB) *IntentTreeHandler {
	return &IntentTreeHandler{db: db}
}

func (h *IntentTreeHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/intent-tree/trees", h.tree)
	rg.POST("/intent-tree", h.create)
	rg.PUT("/intent-tree/:id", h.update)
	rg.DELETE("/intent-tree/:id", h.delete)
	rg.POST("/intent-tree/batch/enable", h.batchEnable)
	rg.POST("/intent-tree/batch/disable", h.batchDisable)
	rg.POST("/intent-tree/batch/delete", h.batchDelete)
}

func (h *IntentTreeHandler) tree(c *gin.Context) {
	var nodes []entity.IntentNodeDO
	h.db.Where("deleted = ?", 0).Order("sort_order ASC").Find(&nodes)
	tree := buildIntentTree(nodes)
	response.Success(c, tree)
}

func buildIntentTree(nodes []entity.IntentNodeDO) []IntentNodeVO {
	codeMap := make(map[string]*IntentNodeVO, len(nodes))
	var voList []*IntentNodeVO
	for i := range nodes {
		n := &nodes[i]
		vo := &IntentNodeVO{
			ID:                  n.ID,
			ParentCode:          n.ParentCode,
			IntentCode:          n.IntentCode,
			Name:                n.Name,
			Level:               n.Level,
			KBID:                n.KBID,
			SortOrder:           n.SortOrder,
			Enabled:             n.Enabled,
			Description:         n.Description,
			Examples:            n.Examples,
			CollectionName:      n.CollectionName,
			TopK:                n.TopK,
			Kind:                n.Kind,
			McpToolID:           n.McpToolID,
			PromptSnippet:       n.PromptSnippet,
			PromptTemplate:      n.PromptTemplate,
			ParamPromptTemplate: n.ParamPromptTemplate,
		}
		codeMap[n.IntentCode] = vo
		voList = append(voList, vo)
	}
	var roots []IntentNodeVO
	for _, vo := range voList {
		if vo.ParentCode == "" {
			roots = append(roots, *vo)
		} else if parent, ok := codeMap[vo.ParentCode]; ok {
			parent.Children = append(parent.Children, *vo)
		} else {
			roots = append(roots, *vo)
		}
	}
	if roots == nil {
		roots = []IntentNodeVO{}
	}
	return roots
}

type IntentNodeVO struct {
	ID                  string         `json:"id"`
	ParentCode          string         `json:"parentCode,omitempty"`
	IntentCode          string         `json:"intentCode"`
	Name                string         `json:"name"`
	Level               int            `json:"level"`
	KBID                string         `json:"kbId,omitempty"`
	SortOrder           int            `json:"sortOrder"`
	Enabled             int            `json:"enabled"`
	Description         string         `json:"description,omitempty"`
	Examples            string         `json:"examples,omitempty"`
	CollectionName      string         `json:"collectionName,omitempty"`
	TopK                *int           `json:"topK,omitempty"`
	Kind                int            `json:"kind"`
	McpToolID           string         `json:"mcpToolId,omitempty"`
	PromptSnippet       string         `json:"promptSnippet,omitempty"`
	PromptTemplate      string         `json:"promptTemplate,omitempty"`
	ParamPromptTemplate string         `json:"paramPromptTemplate,omitempty"`
	Children            []IntentNodeVO `json:"children,omitempty"`
}

func (h *IntentTreeHandler) create(c *gin.Context) {
	var req struct {
		IntentCode          string   `json:"intentCode"`
		ParentCode          string   `json:"parentCode"`
		Name                string   `json:"name"`
		Level               *int     `json:"level"`
		Kind                *int     `json:"kind"`
		KBID                string   `json:"kbId"`
		McpToolID           string   `json:"mcpToolId"`
		SortOrder           *int     `json:"sortOrder"`
		Enabled             *int     `json:"enabled"`
		Description         string   `json:"description"`
		Examples            []string `json:"examples"`
		TopK                *int     `json:"topK"`
		PromptSnippet       string   `json:"promptSnippet"`
		PromptTemplate      string   `json:"promptTemplate"`
		ParamPromptTemplate string   `json:"paramPromptTemplate"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	// Validate: intentCode must be unique
	var cnt int64
	h.db.Model(&entity.IntentNodeDO{}).Where("intent_code = ?", req.IntentCode).Count(&cnt)
	if cnt > 0 {
		response.FailWithCode(c, errcode.ClientError, "意图标识已存在: "+req.IntentCode)
		return
	}

	// Resolve collectionName from KB if kbId is provided
	var collectionName string
	if req.KBID != "" {
		var kb entity.KnowledgeBaseDO
		if h.db.Where("id = ? AND deleted = 0", req.KBID).First(&kb).Error == nil {
			collectionName = kb.CollectionName
		}
	}

	// Convert examples to JSON string
	var examplesJSON string
	if len(req.Examples) > 0 {
		b, _ := json.Marshal(req.Examples)
		examplesJSON = string(b)
	}

	level := 0
	if req.Level != nil {
		level = *req.Level
	}
	kind := 0
	if req.Kind != nil {
		kind = *req.Kind
	}
	sortOrder := 0
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	enabled := 1
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	node := entity.IntentNodeDO{
		BaseModel:           entity.BaseModel{ID: idgen.NextIDStr()},
		ParentCode:          req.ParentCode,
		IntentCode:          req.IntentCode,
		Name:                req.Name,
		Level:               level,
		Kind:                kind,
		KBID:                req.KBID,
		CollectionName:      collectionName,
		McpToolID:           req.McpToolID,
		SortOrder:           sortOrder,
		Enabled:             enabled,
		Description:         req.Description,
		Examples:            examplesJSON,
		TopK:                req.TopK,
		PromptSnippet:       req.PromptSnippet,
		PromptTemplate:      req.PromptTemplate,
		ParamPromptTemplate: req.ParamPromptTemplate,
		CreatedBy:           auth.GetUserID(c.Request.Context()),
	}
	h.db.Create(&node)
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.Success(c, node.ID)
}

func (h *IntentTreeHandler) update(c *gin.Context) {
	id := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	//如果kbid改变了,collectionName也要相应改变，二者是强捆绑的
	if kbID, ok := req["kbId"]; kbID != nil && ok && kbID != "" {
		var kb entity.KnowledgeBaseDO
		if h.db.Where("id = ? AND deleted = 0", id).First(&kb).Error == nil {
			req["collectionName"] = kb.CollectionName
		}
	}
	// Serialize examples if provided as array
	if examples, ok := req["examples"]; ok {
		if arr, isArr := examples.([]interface{}); isArr {
			b, _ := json.Marshal(arr)
			req["examples"] = string(b)
		}
	}
	h.db.Model(&entity.IntentNodeDO{}).Where("id = ? AND deleted = 0", id).Updates(req)
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.SuccessEmpty(c)
}

func (h *IntentTreeHandler) delete(c *gin.Context) {
	id := c.Param("id")
	result := h.db.Model(&entity.IntentNodeDO{}).Where("id = ? AND deleted = 0", id).Update("deleted", 1)
	if result.Error != nil {
		response.FailWithCode(c, errcode.ClientError, "删除失败: "+result.Error.Error())
		return
	}
	if result.RowsAffected == 0 {
		response.FailWithCode(c, errcode.ClientError, "节点不存在或已删除")
		return
	}
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.SuccessEmpty(c)
}

func (h *IntentTreeHandler) batchEnable(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	h.db.Model(&entity.IntentNodeDO{}).Where("id IN ? AND deleted = 0", req.IDs).Update("enabled", 1)
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.SuccessEmpty(c)
}

func (h *IntentTreeHandler) batchDisable(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	//得到目标节点集合和所有节点集合
	targetNodes, allNodes, err := h.listAndValidateTargetNodes(req.IDs)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, err.Error())
		return
	}
	//构筑邻接表
	childrenMap := buildChildrenMap(allNodes)
	//对于每个目标节点，收集他们正在启用的子节点，看有无启用子节点存在阻止关闭
	targetSet := make(map[string]struct{}, len(targetNodes))
	for _, node := range targetNodes {
		targetSet[node.ID] = struct{}{}
	}
	for _, n := range targetNodes {
		desc := collectDescendants(n.IntentCode, childrenMap)
		var enabledButNotSelected []entity.IntentNodeDO
		for _, d := range desc {
			if d.Enabled == 1 && d.ID != n.ID {
				if _, ok := targetSet[d.ID]; !ok {
					enabledButNotSelected = append(enabledButNotSelected, d)
				}
			}
		}
		if len(enabledButNotSelected) > 0 {
			response.FailWithCode(c, errcode.ClientError,
				fmt.Sprintf("批量停用失败：节点 [%s] 存在已启用的子节点未包含在本次操作中（如：%s），请先选择全量子节点",
					displayNodeName(n), summarizeNodeNames(enabledButNotSelected)))
			return
		}
	}
	//disable节点并删除缓存
	targetIDs := make([]string, 0, len(targetNodes))
	for _, node := range targetNodes {
		targetIDs = append(targetIDs, node.ID)
	}
	h.db.Model(&entity.IntentNodeDO{}).Where("id IN ? AND deleted = 0", targetIDs).Update("enabled", 0)
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.SuccessEmpty(c)
}

func (h *IntentTreeHandler) batchDelete(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	//得到目标节点集合和所有节点集合
	targetNodes, allNodes, err := h.listAndValidateTargetNodes(req.IDs)
	if err != nil {
		response.FailWithCode(c, errcode.ClientError, err.Error())
		return
	}
	//构筑邻接表
	childrenMap := buildChildrenMap(allNodes)
	//对于每个目标节点，收集他们正在启用的子节点，看有无启用子节点存在阻止关闭
	targetSet := make(map[string]struct{}, len(targetNodes))
	for _, node := range targetNodes {
		targetSet[node.ID] = struct{}{}
	}
	for _, n := range targetNodes {
		desc := collectDescendants(n.IntentCode, childrenMap)
		var enabledButNotSelected []entity.IntentNodeDO
		var notSelected []entity.IntentNodeDO
		for _, d := range desc {
			if d.Enabled == 1 && d.ID != n.ID {
				if _, ok := targetSet[d.ID]; !ok {
					enabledButNotSelected = append(enabledButNotSelected, d)
				}
			}
			if _, ok := targetSet[d.ID]; !ok {
				notSelected = append(notSelected, d)
			}
		}
		if len(enabledButNotSelected) > 0 {
			response.FailWithCode(c, errcode.ClientError,
				fmt.Sprintf("批量删除失败：节点 [%s] 存在已启用的子节点未包含在本次操作中（如：%s），请先选择全量子节点",
					displayNodeName(n), summarizeNodeNames(enabledButNotSelected)))
			return
		}
		if len(notSelected) > 0 {
			response.FailWithCode(c, errcode.ClientError,
				fmt.Sprintf("批量删除失败：节点 [%s] 未包含全量子节点（如：%s），请先勾选完整子树后再删除",
					displayNodeName(n), summarizeNodeNames(notSelected)))
			return
		}
	}
	targetIDs := make([]string, 0, len(targetNodes))
	for _, n := range targetNodes {
		targetIDs = append(targetIDs, n.ID)
	}
	h.db.Model(&entity.IntentNodeDO{}).Where("id IN ? AND deleted = 0", targetIDs).Update("deleted", 1)
	if cache := intent.DefaultTreeCache(); cache != nil {
		cache.InvalidateCache(c.Request.Context())
	}
	response.SuccessEmpty(c)
}

// 避免重复 ID、空字符串导致误判或重复处理。
func normalizeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// 先拿清洗后的 ID，读出全量未删除节点，再校验目标 ID 是否都真实存在。
// 给后续“树校验”准备可靠输入，先把“节点不存在/已删”挡掉。
func (h *IntentTreeHandler) listAndValidateTargetNodes(ids []string) ([]entity.IntentNodeDO, []entity.IntentNodeDO, error) {
	normalized := normalizeIDs(ids)
	if len(normalized) == 0 {
		return nil, nil, fmt.Errorf("请至少选择一个节点")
	}
	var allNodes []entity.IntentNodeDO

	if err := h.db.Where("deleted = ?", 0).Find(&allNodes).Error; err != nil {
		return nil, nil, err
	}
	nodesByID := make(map[string]entity.IntentNodeDO, len(allNodes))
	for _, node := range allNodes {
		nodesByID[node.ID] = node
	}
	targetNodes := make([]entity.IntentNodeDO, 0, len(normalized))
	missing := make([]string, 0)
	for _, id := range normalized {
		if n, ok := nodesByID[id]; ok {
			targetNodes = append(targetNodes, n)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("节点不存在或已删除: %s", strings.Join(missing, ", "))
	}
	return targetNodes, allNodes, nil
}

// 把节点数组转成 parentCode -> []child 的邻接表。
func buildChildrenMap(nodes []entity.IntentNodeDO) map[string][]entity.IntentNodeDO {
	m := make(map[string][]entity.IntentNodeDO, len(nodes))
	for _, n := range nodes {
		parentCode := n.ParentCode
		if parentCode == "" {
			parentCode = "ROOT"
		}
		m[parentCode] = append(m[parentCode], n)
	}
	return m
}

// 从某个节点开始 DFS，把所有后代节点收集出来。
func collectDescendants(intentCode string, childrenMap map[string][]entity.IntentNodeDO) []entity.IntentNodeDO {
	if strings.TrimSpace(intentCode) == "" {
		return nil
	}
	stack := append([]entity.IntentNodeDO{}, childrenMap[intentCode]...)
	var out []entity.IntentNodeDO
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, n)
		children := childrenMap[n.IntentCode]
		if len(children) > 0 {
			stack = append(stack, children...)
		}
	}
	return out
}

// 把违规节点名压缩成可读提示（最多几个名字）。
func summarizeNodeNames(nodes []entity.IntentNodeDO) string {
	if len(nodes) == 0 {
		return ""
	}
	limit := 3
	if len(nodes) < limit {
		limit = len(nodes)
	}
	names := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		names = append(names, displayNodeName(nodes[i]))
	}
	return strings.Join(names, "、")
}

func displayNodeName(n entity.IntentNodeDO) string {
	if strings.TrimSpace(n.Name) != "" {
		return n.Name
	}
	return n.IntentCode
}

type MappingHandler struct {
	db         *gorm.DB
	termMapper *rewrite.TermMapper
}

func NewMappingHandler(db *gorm.DB, termMapper *rewrite.TermMapper) *MappingHandler {
	return &MappingHandler{db: db, termMapper: termMapper}
}

func (h *MappingHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/mappings", h.page)
	rg.GET("/mappings/:id", h.get)
	rg.POST("/mappings", h.create)
	rg.PUT("/mappings/:id", h.update)
	rg.DELETE("/mappings/:id", h.delete)
}

func (h *MappingHandler) page(c *gin.Context) {
	pageNo, size := response.ParsePage(c)
	var list []entity.QueryTermMappingDO
	var total int64
	h.db.Model(&entity.QueryTermMappingDO{}).Where("deleted = ?", 0).Count(&total)
	h.db.Where("deleted = ?", 0).Offset((pageNo - 1) * size).Limit(size).Order("create_time DESC").Find(&list)
	response.SuccessPage(c, list, total, pageNo, size)
}

func (h *MappingHandler) get(c *gin.Context) {
	id := c.Param("id")
	var m entity.QueryTermMappingDO
	if err := h.db.Where("id = ? AND deleted = ?", id, 0).First(&m).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "mapping not found")
		return
	}
	response.Success(c, m)
}

func (h *MappingHandler) create(c *gin.Context) {
	var req struct {
		SourceTerm string      `json:"sourceTerm" binding:"required"`
		TargetTerm string      `json:"targetTerm" binding:"required"`
		MatchType  *int        `json:"matchType"`
		Priority   *int        `json:"priority"`
		Enabled    flexBoolInt `json:"enabled"`
		Remark     string      `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误: "+err.Error())
		return
	}
	matchType := 1
	if req.MatchType != nil {
		matchType = *req.MatchType
	}
	priority := 100
	if req.Priority != nil {
		priority = *req.Priority
	}
	enabled := 1
	if p := req.Enabled.Ptr(); p != nil {
		enabled = *p
	}
	m := entity.QueryTermMappingDO{
		BaseModel:  entity.BaseModel{ID: idgen.NextIDStr()},
		SourceTerm: req.SourceTerm,
		TargetTerm: req.TargetTerm,
		MatchType:  matchType,
		Priority:   priority,
		Enabled:    enabled,
		Remark:     req.Remark,
		CreatedBy:  auth.GetUserID(c.Request.Context()),
		UpdatedBy:  auth.GetUserID(c.Request.Context()),
	}
	h.db.Create(&m)
	h.reloadTermMapper()
	response.Success(c, m.ID)
}

func (h *MappingHandler) update(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		SourceTerm *string     `json:"sourceTerm"`
		TargetTerm *string     `json:"targetTerm"`
		MatchType  *int        `json:"matchType"`
		Priority   *int        `json:"priority"`
		Enabled    flexBoolInt `json:"enabled"`
		Remark     *string     `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}
	updates := map[string]interface{}{
		"update_by": auth.GetUserID(c.Request.Context()),
	}
	if req.SourceTerm != nil {
		updates["source_term"] = *req.SourceTerm
	}
	if req.TargetTerm != nil {
		updates["target_term"] = *req.TargetTerm
	}
	if req.MatchType != nil {
		updates["match_type"] = *req.MatchType
	}
	if req.Priority != nil {
		updates["priority"] = *req.Priority
	}
	if p := req.Enabled.Ptr(); p != nil {
		updates["enabled"] = *p
	}
	if req.Remark != nil {
		updates["remark"] = *req.Remark
	}
	h.db.Model(&entity.QueryTermMappingDO{}).Where("id = ? AND deleted = 0", id).Updates(updates)
	h.reloadTermMapper()
	response.SuccessEmpty(c)
}

func (h *MappingHandler) delete(c *gin.Context) {
	id := c.Param("id")
	h.db.Model(&entity.QueryTermMappingDO{}).Where("id = ? AND deleted = 0", id).Update("deleted", 1)
	h.reloadTermMapper()
	response.SuccessEmpty(c)
}

// reloadTermMapper reloads all enabled mappings from DB into the termMapper.
// It selects source_term, target_term, match_type and priority so the mapper
// can correctly apply priority ordering and matchType filtering.
func (h *MappingHandler) reloadTermMapper() {
	type termMappingRow struct {
		SourceTerm string
		TargetTerm string
		MatchType  int
		Priority   int
	}
	var records []termMappingRow
	h.db.Model(&entity.QueryTermMappingDO{}).
		Select("source_term, target_term, match_type, priority").
		Where("deleted = 0 AND  CAST(enabled AS TEXT) IN  ('1','true','t')").
		Order("priority DESC").
		Find(&records)
	mappings := make([]rewrite.TermMapping, 0, len(records))
	for _, r := range records {
		mappings = append(mappings, rewrite.TermMapping{
			Original:   r.SourceTerm,
			Normalized: r.TargetTerm,
			MatchType:  r.MatchType,
			Priority:   r.Priority,
		})
	}
	h.termMapper.ReloadMappings(mappings)
}

type SampleQuestionHandler struct{ db *gorm.DB }

func NewSampleQuestionHandler(db *gorm.DB) *SampleQuestionHandler {
	return &SampleQuestionHandler{db: db}
}

func (h *SampleQuestionHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/rag/sample-questions", h.random) // 前端首页随机问题
	rg.GET("/sample-questions", h.page)
	rg.GET("/sample-questions/random", h.random) // 保留别名
	rg.GET("/sample-questions/:id", h.get)
	rg.POST("/sample-questions", h.create)
	rg.PUT("/sample-questions/:id", h.update)
	rg.DELETE("/sample-questions/:id", h.delete)
}

func (h *SampleQuestionHandler) page(c *gin.Context) {
	pageNo, size := response.ParsePage(c)
	keyword := strings.TrimSpace(c.Query("keyword"))
	var list []entity.SampleQuestionDO
	var total int64
	q := h.db.Model(&entity.SampleQuestionDO{}).Where("deleted = 0")
	if keyword != "" {
		q = q.Where("question LIKE ? OR title LIKE ?", "%"+keyword+"%", "%"+keyword+"%")
	}
	q.Count(&total).Offset((pageNo - 1) * size).Limit(size).Find(&list)
	response.SuccessPage(c, list, total, pageNo, size)
}

func (h *SampleQuestionHandler) random(c *gin.Context) {
	var list []entity.SampleQuestionDO
	h.db.Where("deleted = 0").Order("RANDOM()").Limit(6).Find(&list)
	response.Success(c, list)
}

func (h *SampleQuestionHandler) get(c *gin.Context) {
	id := c.Param("id")
	var sq entity.SampleQuestionDO
	if err := h.db.Where("id = ? AND deleted = 0", id).First(&sq).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "question not found")
		return
	}
	response.Success(c, sq)
}

func (h *SampleQuestionHandler) create(c *gin.Context) {
	var req struct {
		Category string `json:"category"`
		Question string `json:"question"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	sq := entity.SampleQuestionDO{
		BaseModel:   entity.BaseModel{ID: idgen.NextIDStr()},
		Title:       req.Category,
		Description: "",
		Question:    req.Question,
	}
	h.db.Create(&sq)
	response.Success(c, sq)
}

func (h *SampleQuestionHandler) update(c *gin.Context) {
	id := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	// 自动设置 updated_by
	req["update_by"] = auth.GetUserID(c.Request.Context())
	result := h.db.Model(&entity.SampleQuestionDO{}).Where("id = ? AND deleted = 0", id).Updates(req)
	if result.Error != nil {
		response.FailWithCode(c, errcode.ClientError, "更新失败: "+result.Error.Error())
		return
	}
	if result.RowsAffected == 0 {
		response.FailWithCode(c, errcode.ClientError, "问题不存在或已删除")
		return
	}
	response.SuccessEmpty(c)
}

func (h *SampleQuestionHandler) delete(c *gin.Context) {
	id := c.Param("id")
	result := h.db.Model(&entity.SampleQuestionDO{}).Where("id = ? AND deleted = 0", id).Updates(map[string]interface{}{
		"deleted":    1,
		"updated_by": auth.GetUserID(c.Request.Context()),
	})
	if result.Error != nil {
		response.FailWithCode(c, errcode.ClientError, "删除失败: "+result.Error.Error())
		return
	}
	if result.RowsAffected == 0 {
		response.FailWithCode(c, errcode.ClientError, "问题不存在或已删除")
		return
	}
	response.SuccessEmpty(c)
}
