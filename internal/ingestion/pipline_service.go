package ingestion

// PipelineService：主表存 pipeline 元数据，子表每行存一个节点（node_id + next + settings/condition JSON）。
// 任务表引 pipeline_id；执行时 GetDefinition → engine，与「管理页展示」共用子表数据但排序语义不同（见 GetDefinition vs loadNodeViews）。

import (
	"context"
	"encoding/json"
	"errors"
	"gogent/internal/entity"
	"gogent/pkg/errcode"
	"strings"

	"gorm.io/gorm"
)

type PipelineMutation struct {
	Name        string       `json:"name"`
	Description *string      `json:"description"`
	Nodes       []NodeConfig `json:"nodes"`
}

// PipelineNodeView 多一个 ID：子表主键，前端编辑/删除单节点时可能用到；NodeConfig 里只有 nodeId 业务键。
type PipelineNodeView struct {
	ID         string          `json:"id"`
	NodeID     string          `json:"nodeId"`
	NodeType   string          `json:"nodeType"`
	Settings   json.RawMessage `json:"settings,omitempty"`
	Condition  json.RawMessage `json:"condition,omitempty"`
	NextNodeID string          `json:"nextNodeId,omitempty"`
}

type PipelineView struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	CreatedBy   string             `json:"createdBy,omitempty"`
	CreateTime  interface{}        `json:"createTime,omitempty"` // GORM/驱动可能回传 time 或 string，用 interface 避免强类型与前端 JSON 不兼容
	UpdateTime  interface{}        `json:"updateTime,omitempty"`
	Nodes       []PipelineNodeView `json:"nodes"`
}

type PipelineService struct {
	db *gorm.DB
}

// NewPipelineService 创建 pipeline 定义服务，所有操作都围绕 ingestion pipeline 表和节点表。
func NewPipelineService(db *gorm.DB) *PipelineService {
	return &PipelineService{db: db}
}

// Create 创建一条 pipeline 定义，并在同一事务中写入全部节点。
// 使用事务是为了避免“主表创建成功但节点写入失败”的半成品 pipeline。
func (s *PipelineService) Create(ctx context.Context, userID string, req PipelineMutation) (*PipelineView, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errcode.NewClientError("pipeline name is required")
	}
	var pipelineID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// pipeline 主表只保存名称、描述和创建人，节点拓扑在子表中维护。
		pipeline := entity.IngestionPipelineDO{
			BaseModel:   entity.BaseModel{ID: nextID()},
			Name:        name,
			Description: stringValue(req.Description),
			CreatedBy:   userID,
			UpdatedBy:   userID,
		}
		if err := tx.Create(&pipeline).Error; err != nil {
			if isDuplicateKey(err) {
				return errcode.NewClientError("流水线名称已存在")
			}
			return err
		}
		pipelineID = pipeline.ID
		// 节点写入失败会回滚主表，保证定义和节点一致。
		return replaceNodesInTx(ctx, tx, pipeline.ID, userID, req.Nodes)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, pipelineID)
}

// Update 更新 pipeline 主表；updateNodes 为 true 时会整体替换节点列表。
// 整体替换比逐个 patch 更简单，也更符合前端画布一次提交完整拓扑的模式。
func (s *PipelineService) Update(ctx context.Context, id string, userID string, req PipelineMutation, updateNodes bool) (*PipelineView, error) {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pipeline entity.IngestionPipelineDO
		if err := tx.Where("id = ? AND deleted = 0", id).First(&pipeline).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errcode.NewClientError("未找到流水线")
			}
			return err
		}
		updates := map[string]interface{}{"updated_by": userID}
		if strings.TrimSpace(req.Name) != "" {
			updates["name"] = strings.TrimSpace(req.Name)
		}
		if req.Description != nil {
			updates["description"] = *req.Description
		}
		if err := tx.Model(&entity.IngestionPipelineDO{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			if isDuplicateKey(err) {
				return errcode.NewClientError("流水线名称已存在")
			}
			return err
		}
		if updateNodes {
			// 注意：ingestion_handler 在 nodes==nil 时只改主表、不触达本分支；为 true 时**整表替换**子表行
			return replaceNodesInTx(ctx, tx, id, userID, req.Nodes)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Get 查询单条 pipeline，并加载排序后的节点视图。
func (s *PipelineService) Get(ctx context.Context, id string) (*PipelineView, error) {
	var pipeline entity.IngestionPipelineDO
	if err := s.db.WithContext(ctx).Where("id = ? AND deleted = 0", id).First(&pipeline).Error; err != nil {
		return nil, err
	}
	nodes, err := s.loadNodeViews(ctx, id)
	if err != nil {
		return nil, err
	}
	return &PipelineView{
		ID:          pipeline.ID,
		Name:        pipeline.Name,
		Description: pipeline.Description,
		CreatedBy:   pipeline.CreatedBy,
		CreateTime:  pipeline.CreateTime,
		UpdateTime:  pipeline.UpdateTime,
		Nodes:       nodes,
	}, nil
}

// Delete：主表软删（可审计/可恢复策略由表设计决定）；子表**物理删**——再建 pipeline 会重新 insert 新行，不保留历史节点版本。
func (s *PipelineService) Delete(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&entity.IngestionPipelineDO{}).Where("id = ? AND deleted = 0", id).Update("deleted", 1).Error; err != nil {
			return err
		}
		return tx.Where("pipeline_id = ?", id).Delete(&entity.IngestionPipelineNodeDO{}).Error
	})
}

// Page 分页查询 pipeline 管理列表，并为每条记录加载节点摘要。
func (s *PipelineService) Page(ctx context.Context, pageNo, pageSize int, keyword string) ([]PipelineView, int64, error) {
	var list []entity.IngestionPipelineDO
	var total int64
	query := s.db.WithContext(ctx).Model(&entity.IngestionPipelineDO{}).Where("deleted = 0")
	if strings.TrimSpace(keyword) != "" {
		query = query.Where("name LIKE ?", "%"+strings.TrimSpace(keyword)+"%")
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := query.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("create_time DESC").Find(&list).Error; err != nil {
		return nil, 0, err
	}
	views := make([]PipelineView, 0, len(list))
	for _, pipeline := range list {
		nodes, err := s.loadNodeViews(ctx, pipeline.ID)
		if err != nil {
			return nil, 0, err
		}
		views = append(views, PipelineView{
			ID:          pipeline.ID,
			Name:        pipeline.Name,
			Description: pipeline.Description,
			CreatedBy:   pipeline.CreatedBy,
			CreateTime:  pipeline.CreateTime,
			UpdateTime:  pipeline.UpdateTime,
			Nodes:       nodes,
		})
	}
	return views, total, nil
}

// GetDefinition 将数据库记录还原成 engine 可执行的 PipelineDefinition。
// 和 Get 的区别是：Get 面向 API 展示，GetDefinition 面向 runtime 执行。
func (s *PipelineService) GetDefinition(ctx context.Context, id string) (PipelineDefinition, error) {
	var pipeline entity.IngestionPipelineDO
	if err := s.db.WithContext(ctx).Where("id = ? AND deleted = 0", id).First(&pipeline).Error; err != nil {
		return PipelineDefinition{}, err
	}
	var nodeDOs []entity.IngestionPipelineNodeDO
	if err := s.db.WithContext(ctx).Where("pipeline_id = ? AND deleted = 0", id).Find(&nodeDOs).Error; err != nil {
		return PipelineDefinition{}, err
	}
	nodes := make([]NodeConfig, 0, len(nodeDOs))
	for _, node := range nodeDOs {
		// 库中 TEXT 列经 parseJSONText 成 json.RawMessage；空串→nil，节点内按「无配置」走默认行为
		nodes = append(nodes, NodeConfig{
			NodeID:     node.NodeID,
			NodeType:   normalizeNodeType(node.NodeType),
			Settings:   parseJSONText(node.SettingsJson),
			Condition:  parseJSONText(node.ConditionJson),
			NextNodeID: node.NextNodeID,
		})
	}
	// 与 API 的 loadNodeViews 不同：执行路径上拓扑非法必须**直接失败**，避免 engine 在运行时才爆
	ordered, err := OrderPipelineNodes(nodes)
	if err != nil {
		return PipelineDefinition{}, err
	}
	return PipelineDefinition{
		ID:          pipeline.ID,
		Name:        pipeline.Name,
		Description: pipeline.Description,
		Nodes:       ordered,
	}, nil
}

// replaceNodes 是非事务场景下的节点整体替换入口，内部复用 replaceNodesInTx。
func (s *PipelineService) replaceNodes(ctx context.Context, pipelineID string, userID string, nodes []NodeConfig) error {
	return replaceNodesInTx(ctx, s.db, pipelineID, userID, nodes)
}

// replaceNodesInTx：先清 pipeline 下全部节点行再全量插入——等价于「整图覆盖」，不做行级 diff。
func replaceNodesInTx(ctx context.Context, db *gorm.DB, pipelineID string, userID string, nodes []NodeConfig) error {
	// 子表无 deleted 软删时，用 Delete+Create 最简；需保证与任务历史兼容（仅引用 pipeline_id 不依赖节点行 id）。
	if err := db.WithContext(ctx).Where("pipeline_id = ?", pipelineID).Delete(&entity.IngestionPipelineNodeDO{}).Error; err != nil {
		return err
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.NodeID) == "" {
			// 空 nodeID 无法参与拓扑排序，直接忽略这类无效节点。
			continue
		}
		nodeType, err := ParseIngestionNodeType(node.NodeType)
		if err != nil {
			return errcode.NewClientError("未知节点类型: " + node.NodeType)
		}
		record := entity.IngestionPipelineNodeDO{
			BaseModel:     entity.BaseModel{ID: nextID()},
			PipelineID:    pipelineID,
			NodeID:        strings.TrimSpace(node.NodeID),
			NodeType:      string(nodeType),
			NextNodeID:    strings.TrimSpace(node.NextNodeID),
			SettingsJson:  marshalJSONText(node.Settings),
			ConditionJson: marshalJSONText(node.Condition),
		}
		if err := db.WithContext(ctx).Create(&record).Error; err != nil {
			return err
		}
		// userID 保留在签名中，方便后续需要写节点级 created_by/updated_by 时扩展。
		_ = userID
	}
	return nil
}

// isDuplicateKey：兼容 Postgres(23505)、MySQL 文案、SQLite UNIQUE failed；跨库判重名的脆弱点集中在此，便于单测/替换。
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "UNIQUE constraint failed")
}

// loadNodeViews：管理端要「能打开编辑页」；若 next 链有环/断链，OrderPipelineNodes 失败则**回退为 DB 查询顺序**（通常即插入顺序），便于人工修正。
func (s *PipelineService) loadNodeViews(ctx context.Context, pipelineID string) ([]PipelineNodeView, error) {
	var nodeDOs []entity.IngestionPipelineNodeDO
	if err := s.db.WithContext(ctx).Where("pipeline_id = ? AND deleted = 0", pipelineID).Find(&nodeDOs).Error; err != nil {
		return nil, err
	}
	nodes := make([]NodeConfig, 0, len(nodeDOs))
	idByNodeID := make(map[string]string, len(nodeDOs))
	for _, node := range nodeDOs {
		nodes = append(nodes, NodeConfig{
			NodeID:     node.NodeID,
			NodeType:   normalizeNodeType(node.NodeType),
			Settings:   parseJSONText(node.SettingsJson),
			Condition:  parseJSONText(node.ConditionJson),
			NextNodeID: node.NextNodeID,
		})
		idByNodeID[node.NodeID] = node.ID
	}
	ordered, err := OrderPipelineNodes(nodes)
	if err != nil {
		// 这里 err 来自拓扑（环、悬空 next 等），不是 gorm.ErrRecordNotFound；保留 err 时仍能展示比直接 500 更有价值
		ordered = nodes
	}
	views := make([]PipelineNodeView, 0, len(ordered))
	for _, node := range ordered {
		views = append(views, PipelineNodeView{
			ID:         idByNodeID[node.NodeID],
			NodeID:     node.NodeID,
			NodeType:   normalizeNodeType(node.NodeType),
			Settings:   node.Settings,
			Condition:  node.Condition,
			NextNodeID: node.NextNodeID,
		})
	}
	return views, nil
}
