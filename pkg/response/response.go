package response

import (
	"errors"
	"gogent/pkg/errcode"
	"gogent/pkg/idgen"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const SuccessCode = "0"

type Result struct {
	Code      string      `json:"code"`
	Message   string      `json:"message,omitempty"`
	Data      interface{} `json:"data,omitempty"`
	RequestID string      `json:"requestId,omitempty"`
}

type PageResult struct {
	Records interface{} `json:"records"`
	Total   int64       `json:"total"`
	Size    int         `json:"size"`
	Current int         `json:"current"`
	Pages   int64       `json:"pages"`
}

// NewPageResult creates a PageResult.
func NewPageResult(records interface{}, total int64, pageNo, pageSize int) PageResult {
	pages := total / int64(pageSize)
	if total%int64(pageSize) > 0 {
		pages++
	}
	return PageResult{
		Records: records,
		Total:   total,
		Size:    pageSize,
		Current: pageNo,
		Pages:   pages,
	}
}

func reqID() string {
	return idgen.NextIDStr()
}

// Success returns a success response with data.
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Result{
		Code:      SuccessCode,
		Data:      data,
		RequestID: reqID(),
	})
}

// SuccessEmpty returns a success response without data.
func SuccessEmpty(c *gin.Context) {
	c.JSON(http.StatusOK, Result{Code: SuccessCode, RequestID: reqID()})
}

// SuccessPage returns a paginated success response.
func SuccessPage(c *gin.Context, records interface{}, total int64, pageNo, pageSize int) {
	c.JSON(http.StatusOK, Result{
		Code:      SuccessCode,
		Data:      NewPageResult(records, total, pageNo, pageSize),
		RequestID: reqID(),
	})
}

// Fail returns an error response, auto-detecting AppError.
func Fail(c *gin.Context, err error) {
	var appErr *errcode.AppError
	if errors.As(err, &appErr) {
		msg := appErr.Msg
		if msg == "" {
			msg = appErr.ErrorCode.Code
		}
		c.JSON(http.StatusOK, Result{
			Code:      appErr.ErrorCode.Code,
			Message:   msg,
			RequestID: reqID(),
		})
		return
	}
	c.JSON(http.StatusOK, Result{
		Code:      errcode.ServiceError.Code,
		Message:   err.Error(),
		RequestID: reqID(),
	})
}

// FailWithCode returns an error response with explicit code.
func FailWithCode(c *gin.Context, code errcode.ErrorCode, msg string) {
	c.JSON(http.StatusOK, Result{
		Code:      code.Code,
		Message:   msg,
		RequestID: reqID(),
	})
}

// ParsePage extracts pagination params from query, supporting both
// frontend conventions: current/size (most services) and pageNo/pageSize (ingestion).
func ParsePage(c *gin.Context) (pageNo, pageSize int) {
	pageNo = 1
	pageSize = 10
	if v := c.Query("current"); v != "" {
		pageNo, _ = strconv.Atoi(v)
	} else if v := c.Query("pageNo"); v != "" {
		pageNo, _ = strconv.Atoi(v)
	}
	if v := c.Query("size"); v != "" {
		pageSize, _ = strconv.Atoi(v)
	} else if v := c.Query("pageSize"); v != "" {
		pageSize, _ = strconv.Atoi(v)
	}
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	return
}
