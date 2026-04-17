package middleware

import (
	"gogent/pkg/errcode"
	"gogent/pkg/response"
	"net/http"
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
		// Only protect write operations.
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}

		// Frontend does not send idempotent token for most requests.
		// Keep Java-compatible behavior by enforcing dedup only when token exists.
		if rdb == nil {
			c.Next()
			return
		}

		// 1. Get idempotent token from header
		token := c.GetHeader("Idempotent-Token")
		if token == "" {
			c.Next()
			return
		}

		// 2. Build Redis key
		key := "idempotent:submit:" + token

		// 3. Try to set token (SET NX = only set if not exists)
		ctx := c.Request.Context()
		success, err := rdb.SetNX(ctx, key, "1", 5*time.Minute).Result()
		if err != nil {
			response.Fail(c, err)
			c.Abort()
			return
		}

		// 4. If key already exists, this is a duplicate request
		if !success {
			response.FailWithCode(c, errcode.IdempotentTokenDeleteError, "您操作太快，请稍后再试")
			c.Abort()
			return
		}

		// 5. Token is new, continue processing
		c.Next()
	}
}
