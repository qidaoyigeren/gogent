package handler

import (
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const defaultAdminUsername = "admin"

type UserHandler struct {
	db *gorm.DB
}

func NewUserHandler(db *gorm.DB) *UserHandler {
	return &UserHandler{db: db}
}

func (h *UserHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/user/me", h.currentUser)
	rg.PUT("/user/password", h.changePassword)
	// Admin only routes (matching Java: StpUtil.checkRole("admin"))
	rg.GET("/users", auth.RequireRole("admin"), h.page)
	rg.POST("/users", auth.RequireRole("admin"), h.create)
	rg.PUT("/users/:id", auth.RequireRole("admin"), h.update)
	rg.DELETE("/users/:id", auth.RequireRole("admin"), h.delete)
}

func (h *UserHandler) currentUser(c *gin.Context) {
	user := auth.GetUser(c.Request.Context())
	if user == nil {
		response.FailWithCode(c, errcode.ClientError, "not logged in")
		return
	}
	var u entity.UserDO
	if err := h.db.Where("id = ? AND deleted = 0", user.UserID).First(&u).Error; err == nil {
		user.Avatar = u.Avatar
	}
	response.Success(c, user)
}

func (h *UserHandler) changePassword(c *gin.Context) {
	// Accept both "currentPassword" (frontend) and "oldPassword" (legacy).
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		OldPassword     string `json:"oldPassword"`
		NewPassword     string `json:"newPassword" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	oldPwd := req.CurrentPassword
	if oldPwd == "" {
		oldPwd = req.OldPassword
	}
	if oldPwd == "" {
		response.FailWithCode(c, errcode.ClientError, "current password is required")
		return
	}
	userID := auth.GetUserID(c.Request.Context())
	var user entity.UserDO
	if err := h.db.Where("id = ? AND deleted = 0", userID).First(&user).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "user not found")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(oldPwd)); err != nil {
		response.FailWithCode(c, errcode.ClientError, "wrong old password")
		return
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	h.db.Model(&user).Update("password", string(hashed))
	response.SuccessEmpty(c)
}

func (h *UserHandler) page(c *gin.Context) {
	pageNo, pageSize := response.ParsePage(c)
	keyword := c.Query("keyword")
	var list []entity.UserDO
	var total int64
	q := h.db.Model(&entity.UserDO{}).Where("deleted = 0")
	if keyword != "" {
		q = q.Where("username LIKE ?", "%"+keyword+"%")
	}
	q.Count(&total)
	q.Offset((pageNo - 1) * pageSize).Limit(pageSize).Order("create_time DESC").Find(&list)
	response.SuccessPage(c, list, total, pageNo, pageSize)
}

func (h *UserHandler) create(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
		Role     string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	username := strings.TrimSpace(req.Username)
	var count int64
	h.db.Model(&entity.UserDO{}).Where("username = ? AND deleted = 0", username).Count(&count)
	if count > 0 {
		response.FailWithCode(c, errcode.ClientError, "username already exists")
		return
	}
	role := req.Role
	if role == "" {
		role = "user"
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	user := entity.UserDO{
		BaseModel: entity.BaseModel{ID: idgen.NextIDStr()},
		Username:  username,
		Password:  string(hashed),
		Role:      role,
	}
	h.db.Create(&user)
	response.Success(c, user.ID)
}

func (h *UserHandler) update(c *gin.Context) {
	id := c.Param("id")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "invalid request")
		return
	}
	delete(req, "id")
	delete(req, "password")
	h.db.Model(&entity.UserDO{}).Where("id = ? AND deleted = 0", id).Updates(req)
	response.SuccessEmpty(c)
}

func (h *UserHandler) delete(c *gin.Context) {
	id := c.Param("id")
	h.db.Model(&entity.UserDO{}).Where("id = ? AND deleted = 0", id).Update("deleted", 1)
	response.SuccessEmpty(c)
}
