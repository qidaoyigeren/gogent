package handler

import (
	"fmt"
	"gogent/internal/auth"
	"gogent/internal/entity"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type AuthHandler struct {
	db    *gorm.DB
	redis *redis.Client
}

func NewAuthHandler(db *gorm.DB, redis *redis.Client) *AuthHandler {
	return &AuthHandler{db: db, redis: redis}
}

func (h *AuthHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/login", h.login)
	rg.POST("/auth/logout", h.logout)
}

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type LoginVO struct {
	Token    string `json:"token"`
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Avatar   string `json:"avatar,omitempty"`
}

func (h *AuthHandler) login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailWithCode(c, errcode.ClientError, "请求参数错误")
		return
	}

	var user entity.UserDO
	if err := h.db.Where("username = ? AND deleted = 0", req.Username).First(&user).Error; err != nil {
		response.FailWithCode(c, errcode.ClientError, "用户名或密码错误")
		return
	}

	// Check password: support both BCrypt (Go) and plaintext (Java legacy)
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		// Fallback: check plaintext password (for Java legacy users)
		if user.Password != req.Password {
			response.FailWithCode(c, errcode.ClientError, "用户名或密码错误")
			return
		}
	}

	token, err := auth.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		response.Fail(c, err)
		return
	}

	response.Success(c, LoginVO{
		Token:    token,
		UserID:   user.ID,
		Username: user.Username,
		Role:     user.Role,
		Avatar:   user.Avatar,
	})
}

func (h *AuthHandler) logout(c *gin.Context) {
	// 获取当前 Token
	tokenStr := c.GetHeader("Authorization")
	if len(tokenStr) > 7 && tokenStr[:7] == "Bearer " {
		tokenStr = tokenStr[7:]
	}

	// 将 Token 加入黑名单（直到过期）
	if tokenStr != "" && h.redis != nil {
		ctx := c.Request.Context()
		// 解析 Token 获取过期时间
		claims, err := auth.ParseToken(tokenStr)
		if err == nil && claims != nil {
			// 计算剩余有效期
			expireTime := time.Until(claims.ExpiresAt.Time)
			if expireTime > 0 {
				// 存入 Redis 黑名单
				blacklistKey := fmt.Sprintf("jwt:blacklist:%s", tokenStr)
				h.redis.Set(ctx, blacklistKey, "1", expireTime)
			}
		}
	}

	response.SuccessEmpty(c)
}

// EnsureDefaultAdmin creates a default admin user if none exists.
func EnsureDefaultAdmin(db *gorm.DB) {
	var count int64
	db.Model(&entity.UserDO{}).Count(&count)
	if count > 0 {
		return
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	db.Create(&entity.UserDO{
		BaseModel: entity.BaseModel{ID: idgen.NextIDStr()},
		Username:  "admin",
		Password:  string(hashed),
		Role:      "admin",
	})
}
