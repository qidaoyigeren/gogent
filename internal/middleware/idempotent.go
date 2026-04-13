package middleware

import (
	"gogent/pkg/errcode"
	"gogent/pkg/response"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// IdempotentMiddleware creates a middleware for idempotent request handling.
// This matches Java's @IdempotentSubmit annotation behavior.
//
// Usage:
//   - Client sends request with "Idempotent-Token" header
//   - Server checks if token already used (via Redis SET NX)
//   - If token exists, reject with 400 error
//   - If token is new, allow request and set expiry (5 minutes)
//
// Example:
//
//	// Register middleware
//	api.Use(middleware.IdempotentMiddleware(rdb))
//
//	// Client request
//	POST /rag/v3/chat
//	Idempotent-Token: abc123-def456
func IdempotentMiddleware(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Idempotent-Token")
		if token == "" {
			response.FailWithCode(c, errcode.IdempotentTokenNullError, "幂等token为空")
			c.Abort()
			return
		}
		key := "idempotent:submit:" + token
		ctx := c.Request.Context()
		success, err := rdb.SetNX(ctx, key, "1", 5*time.Minute).Result()
		if err != nil {
			response.Fail(c, err)
			c.Abort()
			return
		}
		if !success {
			response.FailWithCode(c, errcode.IdempotentTokenDeleteError, "您操作太快，请稍后再试")
			c.Abort()
			return
		}
		c.Next()
	}
}
