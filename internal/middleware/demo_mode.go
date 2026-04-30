// Package middleware provides HTTP middleware for Gin router.
package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"gogent/pkg/response"
)

// DemoModeMiddleware blocks all mutating requests when demo mode is enabled.
// Equivalent to Java's DemoModeInterceptor.
//
// Behavior when enabled:
//   - GET requests and OPTIONS (CORS preflight): pass through
//   - SSE chat path (GET /rag/v3/chat): emits SSE reject + done events
//   - All other non-GET: returns JSON error (A000001)
func DemoModeMiddleware(enabled bool, contextPath string) gin.HandlerFunc {
	if !enabled {
		return func(c *gin.Context) { c.Next() }
	}

	// Normalize context path for SSE path matching
	chatPath := "/rag/v3/chat"
	if trimmed := strings.TrimRight(strings.TrimSpace(contextPath), "/"); trimmed != "" {
		chatPath = trimmed + "/rag/v3/chat"
	}

	const rejectMsg = "体验环境仅支持查询操作"

	return func(c *gin.Context) {
		method := c.Request.Method

		// Always allow CORS preflight
		if method == http.MethodOptions {
			c.Next()
			return
		}

		path := c.Request.URL.Path

		// SSE chat path is GET but must be blocked in demo mode
		isSsePath := path == chatPath
		if method == http.MethodGet && !isSsePath {
			c.Next()
			return
		}

		// Block: SSE path gets an SSE-format rejection
		if isSsePath {
			c.Header("Content-Type", "text/event-stream;charset=UTF-8")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			fmt.Fprintf(c.Writer, "event: reject\ndata: {\"type\":\"response\",\"delta\":\"%s\"}\n\n", rejectMsg)
			fmt.Fprintf(c.Writer, "event: done\ndata: \"[DONE]\"\n\n")
			c.Writer.Flush()
			c.Abort()
			return
		}

		// All other mutating requests: JSON rejection
		c.AbortWithStatusJSON(http.StatusOK, response.Result{
			Code:      errcode.ClientError.Code,
			Message:   rejectMsg,
			RequestID: idgen.NextIDStr(),
		})
	}
}
