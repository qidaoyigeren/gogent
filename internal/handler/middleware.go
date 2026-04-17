package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// CORSMiddleware handles Cross-Origin requests.
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RequestLogMiddleware logs request info.
func RequestLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		status := c.Writer.Status()
		method := c.Request.Method
		path := c.Request.URL.Path

		if status >= 400 {
			gin.DefaultErrorWriter.Write([]byte(
				method + " " + path + " " + http.StatusText(status) + " " + latency.String() + "\n"))
		}
	}
}

// RecoveryMiddleware recovers from panics.
func RecoveryMiddleware() gin.HandlerFunc {
	return gin.Recovery()
}

// uploadSemaphoreLua atomically increments a counter and checks against limit.
// Returns 1 if permit acquired, 0 if limit exceeded.
const uploadSemaphoreLua = `
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local current = redis.call('INCR', key)
redis.call('EXPIRE', key, ttl)
if current > limit then
  redis.call('DECR', key)
  return 0
end
return 1
`

// UploadSemaphoreMiddleware limits concurrent file uploads using a Redis counter.
// It must be registered on the specific upload route (before multipart parsing).
// Equivalent to Java's UploadRateLimitFilter with Redisson RPermitExpirableSemaphore.
func UploadSemaphoreMiddleware(rdb *redis.Client, maxConcurrent int) gin.HandlerFunc {
	const semKey = "ragent:upload:semaphore"
	const ttlSeconds = 60

	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if !strings.Contains(path, "/knowledge-base/") || !strings.HasSuffix(path, "/docs/upload") {
			c.Next()
			return
		}

		if rdb == nil {
			c.Next()
			return
		}

		ctx := c.Request.Context()
		result, err := rdb.Eval(ctx, uploadSemaphoreLua, []string{semKey}, maxConcurrent, ttlSeconds).Result()
		if err != nil {
			// Redis unavailable: allow through
			c.Next()
			return
		}

		acquired, _ := result.(int64)
		if acquired == 0 {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    "429",
				"message": "当前上传人数过多，请稍后再试",
			})
			return
		}

		// Release permit after handler completes
		defer func() {
			rdb.Decr(context.Background(), semKey)
		}()

		c.Next()
	}
}
