package service

import (
	"gogent/internal/entity"

	"gorm.io/gorm"
)

// ========================= 接口面 =========================
// PageRuns   ：分页查询 run 列表，支持按 traceID / conversationID / taskID / status 过滤
// GetDetail  ：一次取 run + nodes（时间线视图用）
// ListNodes  ：只取某个 trace 下的全部 nodes，按 sort_order 再按 start_time 排序

type RagTraceQueryService struct {
	db *gorm.DB
}

func NewRagTraceQueryService(db *gorm.DB) *RagTraceQueryService {
	return &RagTraceQueryService{db: db}
}

// TraceRunPageRequest 分页 + 过滤条件。
// 所有字符串字段留空代表“不过滤”，走数据库侧最简 WHERE。
type TraceRunPageRequest struct {
	PageNo         int
	PageSize       int
	TraceID        string
	ConversationID string
	TaskID         string
	Status         string
}

// TraceRunVO = RunDO + UserName（从 t_user 回填用于前端展示）。
// 使用嵌入结构（匿名字段）+ 扩展字段，JSON 序列化时会自动展平。
type TraceRunVO struct {
	entity.RagTraceRunDO
	UserName string `json:"userName,omitempty"`
}

// TraceDetailVO ：详情页一次吐出 run + nodes 的聚合视图。
type TraceDetailVO struct {
	Run   TraceRunVO              `json:"run"`
	Nodes []entity.RagTraceNodeDO `json:"nodes"`
}

// PageRuns 按条件分页查询 run，再批量填充 userName。
// 排序：start_time DESC（最新的 run 在前）。
// 返回：[]VO, total, err。
func (s *RagTraceQueryService) PageRuns(req TraceRunPageRequest) ([]TraceRunVO, int64, error) {
	var list []entity.RagTraceRunDO
	var total int64
	q := s.db.Model(&entity.RagTraceRunDO{})
	// 所有过滤条件都在同一个 query builder 上叠加
	if req.TraceID != "" {
		q = q.Where("trace_id = ?", req.TraceID)
	}
	if req.ConversationID != "" {
		q = q.Where("conversation_id = ?", req.ConversationID)
	}
	if req.TaskID != "" {
		q = q.Where("task_id = ?", req.TaskID)
	}
	if req.Status != "" {
		q = q.Where("status = ?", req.Status)
	}
	// 先 Count 再 Find：两次 SQL，但可以准确返回 total
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := q.Offset((req.PageNo - 1) * req.PageSize).Limit(req.PageSize).Order("start_time DESC").Find(&list).Error; err != nil {
		return nil, 0, err
	}

	// ---- 批量回填 userName，避免 N+1 ----
	userIDs := make([]string, 0, len(list))
	for _, r := range list {
		if r.UserID != "" {
			userIDs = append(userIDs, r.UserID)
		}
	}
	usernameMap := make(map[string]string)
	if len(userIDs) > 0 {
		var users []entity.UserDO
		// 忽略错误：userName 属于增强字段，查不到也不应影响 trace 列表返回
		_ = s.db.Where("id IN ?", userIDs).Select("id, username").Find(&users).Error
		for _, u := range users {
			usernameMap[u.ID] = u.Username
		}
	}

	vos := make([]TraceRunVO, 0, len(list))
	for _, r := range list {
		vos = append(vos, TraceRunVO{RagTraceRunDO: r, UserName: usernameMap[r.UserID]})
	}
	return vos, total, nil
}

// GetDetail 根据 traceID 取 run + nodes；run 不存在直接返回 err。
// userName 单独查一次（因为只有一条 run）。
func (s *RagTraceQueryService) GetDetail(traceID string) (*TraceDetailVO, error) {
	var run entity.RagTraceRunDO
	if err := s.db.Where("trace_id = ?", traceID).First(&run).Error; err != nil {
		return nil, err
	}
	var username string
	var user entity.UserDO
	// 单条回填用户名；错误也静默（同 PageRuns 容错思路）
	if s.db.Where("id = ?", run.UserID).Select("username").First(&user).Error == nil {
		username = user.Username
	}

	nodes, err := s.ListNodes(traceID)
	if err != nil {
		return nil, err
	}
	return &TraceDetailVO{
		Run:   TraceRunVO{RagTraceRunDO: run, UserName: username},
		Nodes: nodes,
	}, nil
}

// ListNodes 返回某个 trace 下的全部 node。
// 排序：sort_order ASC, start_time ASC —— 先按写入顺序，再按时间兜底，
// 这样即便并行节点（MCP 并发）的 start_time 极度接近，展示也稳定。
func (s *RagTraceQueryService) ListNodes(traceID string) ([]entity.RagTraceNodeDO, error) {
	var nodes []entity.RagTraceNodeDO
	if err := s.db.Where("trace_id = ?", traceID).Order("sort_order ASC, start_time ASC").Find(&nodes).Error; err != nil {
		return nil, err
	}
	return nodes, nil
}
