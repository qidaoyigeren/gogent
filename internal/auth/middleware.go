package auth

import (
	"fmt"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// authResult writes a consistent response.Result JSON body with the given HTTP status.
// This matches the Java Result<T> shape used everywhere else, rather than bare gin.H.
func authResult(c *gin.Context, httpStatus int, code, message string) {
	c.AbortWithStatusJSON(httpStatus, response.Result{
		Code:      code,
		Message:   message,
		RequestID: idgen.NextIDStr(),
	})
}

// AuthMiddleware returns a Gin middleware that validates JWT tokens.
// Excludes paths starting with /auth/ and OPTIONS requests.
func AuthMiddleware(redisClient *redis.Client, contextPath string) gin.HandlerFunc {
	authPrefix := "/auth/"
	if trimmed := strings.TrimSpace(contextPath); trimmed != "" {
		normalized := "/" + strings.Trim(trimmed, "/")
		authPrefix = normalized + "/auth/"
	}
	return func(c *gin.Context) {
		// Skip OPTIONS (CORS preflight)
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}

		// Skip auth paths
		path := c.Request.URL.Path
		if strings.HasPrefix(path, authPrefix) || path == "/health" {
			c.Next()
			return
		}

		// Extract token (support both "Bearer <token>" and "<token>" formats)
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			authResult(c, http.StatusUnauthorized, "A000401", "未提供认证令牌")
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		tokenStr = strings.TrimSpace(tokenStr)

		// Check JWT blacklist
		if redisClient != nil {
			blacklistKey := fmt.Sprintf("jwt:blacklist:%s", tokenStr)
			exists, err := redisClient.Exists(c.Request.Context(), blacklistKey).Result()
			if err == nil && exists > 0 {
				authResult(c, http.StatusUnauthorized, "A000401", "认证令牌已失效")
				return
			}
		}

		claims, err := ParseToken(tokenStr)
		if err != nil {
			authResult(c, http.StatusUnauthorized, "A000401", "认证令牌无效或已过期")
			return
		}

		// Set user into context
		user := &LoginUser{
			UserID:   claims.UserID,
			Username: claims.Username,
			Role:     claims.Role,
		}
		c.Request = c.Request.WithContext(WithUser(c.Request.Context(), user))
		c.Set("loginUser", user)

		c.Next()
	}
}

// RequireRole returns middleware that checks the user has a specific role.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := GetUser(c.Request.Context())
		if user == nil || user.Role != role {
			authResult(c, http.StatusForbidden, "A000403", "权限不足")
			return
		}
		c.Next()
	}
}
