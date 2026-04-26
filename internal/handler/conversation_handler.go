package handler

import (
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ConversationHandler struct {
	db *gorm.DB
}

func NewConversationHandler(db *gorm.DB) *ConversationHandler {
	return &ConversationHandler{db: db}
}

func (h *ConversationHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/conversations", h.list)                      // 会话列表
	rg.PUT("/conversations/:conversationId", h.rename)    // 重命名
	rg.DELETE("/conversations/:conversationId", h.delete) // 软删除
	rg.GET("/conversations/:conversationId/messages", h.listMessages)
}

// list 按用户列出未删除的会话，按 last_time DESC。
// 返回最小字段集（VO）避免把内部 ID 暴露给前端；LastTime 为空时退化用 UpdateTime。
func (h *ConversationHandler) list(c *gin.Context) {
	userID := auth.GetUserID(c.Request.Context())
	var convos []entity.ConversationDO
	h.db.Where("user_id = ? AND deleted = 0", userID).Order("last_time DESC").Find(&convos)

	type ConversationVO struct {
		ConversationID string `json:"conversationId"`
		Title          string `json:"title"`
		LastTime       string `json:"lastTime"`
	}
	var vos []ConversationVO
	for _, co := range convos {
		// 默认用 UpdateTime；若业务侧有 last_time（最后一条消息时间）则优先
		lt := co.UpdateTime.Format("2006-01-02T15:04:05Z")
		if co.LastTime != nil {
			lt = co.LastTime.Format("2006-01-02T15:04:05Z")
		}
		vos = append(vos, ConversationVO{
			ConversationID: co.ConversationID,
			Title:          co.Title,
			LastTime:       lt,
		})
	}
	// nil slice 会序列化成 null，前端更希望空数组
	if vos == nil {
		vos = []ConversationVO{}
	}
	response.Success(c, vos)
}

// rename 允许用户修改会话标题。
// 限制：标题 <= 50 rune（CJK 安全）；先按 (convID, userID) 校验归属，再更新。
func (h *ConversationHandler) rename(c *gin.Context) {
	convID := c.Param("conversationId")
	userID := auth.GetUserID(c.Request.Context())
	var req struct {
		Title string `json:"title" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	// rune 维度限长，防止中文算不够字节的情况
	titleRunes := []rune(req.Title)
	if len(titleRunes) > 50 {
		response.FailWithCode(c, errcode.ClientError, "会话名称长度不能超过50个字符")
		return
	}

	// 归属校验：必须是当前用户的未删除会话
	var conv entity.ConversationDO
	if err := h.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).First(&conv).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "会话不存在")
		return
	}

	h.db.Model(&entity.ConversationDO{}).Where("conversation_id = ? AND deleted = 0", convID).Update("title", req.Title)
	response.SuccessEmpty(c)
}

// delete 会话软删 —— 级联把消息、摘要也软删，保持一致性（下次列表不再出现）。
// 三条 UPDATE 都带 deleted=0 条件，天然幂等。
func (h *ConversationHandler) delete(c *gin.Context) {
	convID := c.Param("conversationId")
	userID := auth.GetUserID(c.Request.Context())

	// 先校验归属（以 conv.ID 精确定位，避免 conversation_id 的 DELETE 误伤）
	var conv entity.ConversationDO
	if err := h.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).First(&conv).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "会话不存在")
		return
	}

	// 三张表级联软删：conversation / message / summary
	h.db.Model(&entity.ConversationDO{}).Where("id = ?", conv.ID).Update("deleted", 1)
	h.db.Model(&entity.ConversationMessageDO{}).Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).Update("deleted", 1)
	h.db.Model(&entity.ConversationSummaryDO{}).Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).Update("deleted", 1)
	response.SuccessEmpty(c)
}

// listMessages 拉取某会话全部消息（含 vote）。
// 返回顺序：create_time ASC（回放视角）；前端按此渲染聊天窗。
// 性能：先批量拿 assistantIDs，再一次 IN 查所有 vote，避免 N+1。
func (h *ConversationHandler) listMessages(c *gin.Context) {
	convID := c.Param("conversationId")
	userID := auth.GetUserID(c.Request.Context())

	// 越权保护：找不到归属时返回空数组
	var conv entity.ConversationDO
	if h.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).First(&conv).Error != nil {
		response.Success(c, []struct{}{})
		return
	}

	var msgs []entity.ConversationMessageDO
	h.db.Where("conversation_id = ? AND user_id = ? AND deleted = 0", convID, userID).Order("create_time ASC").Find(&msgs)

	// 只对 assistant 消息查 vote：user 消息用户不给自己点赞踩
	assistantIDs := make([]string, 0)
	for _, m := range msgs {
		if m.Role == "assistant" {
			assistantIDs = append(assistantIDs, m.ID)
		}
	}
	voteMap := make(map[string]int)
	if len(assistantIDs) > 0 && userID != "" {
		var feedbacks []entity.MessageFeedbackDO
		h.db.Where("user_id = ? AND deleted = 0 AND message_id IN ?", userID, assistantIDs).Find(&feedbacks)
		for _, f := range feedbacks {
			voteMap[f.MessageID] = f.Vote
		}
	}

	// VO 结构：Vote 用 *int 而非 int，好让前端区分“未评价”与“评 0”
	type MessageVO struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversationId"`
		Role           string `json:"role"`
		Content        string `json:"content"`
		Vote           *int   `json:"vote"`
		CreateTime     string `json:"createTime"`
	}
	var vos []MessageVO
	for _, m := range msgs {
		var vote *int
		if v, ok := voteMap[m.ID]; ok {
			vote = &v
		}
		vos = append(vos, MessageVO{
			ID:             m.ID,
			ConversationID: m.ConversationID,
			Role:           m.Role,
			Content:        m.Content,
			Vote:           vote,
			CreateTime:     m.CreateTime.Format("2006-01-02T15:04:05Z"),
		})
	}
	if vos == nil {
		vos = []MessageVO{}
	}
	response.Success(c, vos)
}

// ============================================================
//                      FeedbackHandler
// ============================================================

// FeedbackHandler 处理消息的点赞/踩：仅允许对 assistant 消息操作。
type FeedbackHandler struct {
	db *gorm.DB
}

func NewFeedbackHandler(db *gorm.DB) *FeedbackHandler {
	return &FeedbackHandler{db: db}
}

func (h *FeedbackHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/conversations/messages/:messageId/feedback", h.submitFeedback)
	rg.GET("/conversations/messages/votes", h.getUserVotes)
}

// submitFeedback 提交反馈（upsert）：同一消息再次提交会覆盖上一次的 vote/reason/comment。
// Vote 取值固定为 1 或 -1；0 不合法（前端"取消反馈"应走 DELETE 或独立接口）。
func (h *FeedbackHandler) submitFeedback(c *gin.Context) {
	messageID := c.Param("messageId")
	var req struct {
		Vote    int    `json:"vote" binding:"required"` // 1 或 -1
		Reason  string `json:"reason"`
		Comment string `json:"comment"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	if req.Vote != 1 && req.Vote != -1 {
		response.FailWithCode(c, errcode.ClientError, "反馈值必须为 1 或 -1")
		return
	}

	userID := auth.GetUserID(c.Request.Context())

	// 业务校验：消息必须存在、归当前用户、且为 assistant 角色（对齐 Java loadAssistantMessage）
	var msg entity.ConversationMessageDO
	if err := h.db.Where("id = ? AND user_id = ? AND deleted = 0", messageID, userID).First(&msg).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "消息不存在")
		return
	}
	if msg.Role != "assistant" {
		response.FailWithCode(c, errcode.ClientError, "仅支持对助手消息反馈")
		return
	}

	convID := msg.ConversationID

	// Upsert：先查已有，存在则 UPDATE，不存在则 INSERT。
	// 这里没用 DB 唯一键 ON CONFLICT 是为了保留 submit_time 字段语义。
	var existing entity.MessageFeedbackDO
	err := h.db.Where("message_id = ? AND user_id = ? AND deleted = 0", messageID, userID).First(&existing).Error
	if err == nil {
		h.db.Model(&existing).Updates(map[string]interface{}{
			"vote":        req.Vote,
			"reason":      req.Reason,
			"comment":     req.Comment,
			"submit_time": time.Now().UnixMilli(),
		})
	} else {
		h.db.Create(&entity.MessageFeedbackDO{
			BaseModel:      entity.BaseModel{ID: idgen.NextIDStr()},
			MessageID:      messageID,
			ConversationID: convID,
			UserID:         userID,
			Vote:           req.Vote,
			Reason:         req.Reason,
			Comment:        req.Comment,
		})
	}
	response.SuccessEmpty(c)
}

// getUserVotes 批量查某个用户对一组 messageId 的 vote。
// 接口形态：GET /conversations/messages/votes?messageIds=id1,id2,id3
// 返回：map[messageId]vote，未评价的消息不出现在 map 里。
func (h *FeedbackHandler) getUserVotes(c *gin.Context) {
	userID := auth.GetUserID(c.Request.Context())
	idsStr := c.Query("messageIds")
	if idsStr == "" {
		response.Success(c, map[string]int{})
		return
	}

	var ids []string
	for _, id := range splitAndTrim(idsStr, ",") {
		if id != "" {
			ids = append(ids, id)
		}
	}

	var feedbacks []entity.MessageFeedbackDO
	h.db.Where("user_id = ? AND deleted = 0 AND message_id IN ?", userID, ids).Find(&feedbacks)

	result := make(map[string]int)
	for _, f := range feedbacks {
		result[f.MessageID] = f.Vote
	}
	response.Success(c, result)
}

// ============================================================
// 下面是手写的字符串工具：不引 strings 是为了避免与其他 handler 里的
// split/index 同名函数在同包内冲突（Go 无函数重载）。逻辑与 strings.Split
// / strings.Index / strings.TrimSpace 等价，仅保留本文件私用。
// ============================================================

// splitAndTrim 按 sep 切分，每段 trim 空格并去空。
func splitAndTrim(s, sep string) []string {
	parts := make([]string, 0)
	for _, p := range splitString(s, sep) {
		p = trimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// splitString 简易版 strings.Split；不处理正则。
func splitString(s, sep string) []string {
	result := []string{}
	for {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

// indexOf 返回 sub 在 s 中首次出现的位置；无匹配返回 -1。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// trimSpace 只处理半角空格和 tab；不处理其他空白字符（够用即可）。
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
