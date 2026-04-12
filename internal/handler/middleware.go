package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
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
