package auth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

func AuthMiddleware(redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip OPTIONS (CORS preflight)
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}
		// Skip auth paths
		path := c.Request.URL.Path
		if strings.HasPrefix("api/ragent/auth", path) || strings.HasPrefix("api/ragent/health", path) {
			c.Next()
			return
		}
		// Extract token (support both "Bearer <token>" and "<token>" formats)
		header := c.GetHeader("Authorization")
		if header == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "A000401",
				"message": "未提供认证令牌",
			})
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		token = strings.TrimSpace(token)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "A000401",
				"message": "未提供认证令牌",
			})
		}
		// Check JWT blacklist
		if redisClient != nil {
			blacklistKey := fmt.Sprintf("jwt:blacklist:%s", token)
			exists, err := redisClient.Exists(c.Request.Context(), blacklistKey).Result()
			if err == nil && exists > 0 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"code":    "A000401",
					"message": "认证令牌已失效",
				})
				return
			}
		}
		claims, err := ParseToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "A000401",
				"message": "认证令牌无效",
			})
		}

		// Set user into context
		user := &LoginUser{
			Username: claims.Username,
			Role:     claims.Role,
			UserID:   claims.UserID,
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), user))
		// Also set in Gin context for convenience
		c.Set("loginUser", user)
		c.Next()
	}
}

// RequireRole returns middleware that checks the user has a specific role.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := GetUser(c)
		if user == nil || user.Role != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "A000403",
				"message": "权限不足",
			})
			return
		}
		c.Next()
	}
}
