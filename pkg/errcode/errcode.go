package errcode

import "fmt"

// ErrorCode defines an error with code and message.
type ErrorCode struct {
	Code    string
	Message string
}

func (e ErrorCode) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// AppError is a rich error carrying an ErrorCode.
type AppError struct {
	ErrorCode ErrorCode
	Msg       string // custom message, overrides ErrorCode.Message if non-empty
	Cause     error
}

func (e *AppError) Error() string {
	msg := e.Msg
	if msg == "" {
		msg = e.ErrorCode.Message
	}
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.ErrorCode.Code, msg, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.ErrorCode.Code, msg)
}

func (e *AppError) Unwrap() error { return e.Cause }

// --- A-class: client errors ---

var (
	ClientError                = ErrorCode{"A000001", "用户端错误"}
	UserRegisterError          = ErrorCode{"A000100", "用户注册错误"}
	UserNameVerifyError        = ErrorCode{"A000110", "用户名校验失败"}
	UserNameExistError         = ErrorCode{"A000111", "用户名已存在"}
	UserNameSensitiveError     = ErrorCode{"A000112", "用户名包含敏感词"}
	UserNameSpecialCharError   = ErrorCode{"A000113", "用户名包含特殊字符"}
	PasswordVerifyError        = ErrorCode{"A000120", "密码校验失败"}
	PasswordShortError         = ErrorCode{"A000121", "密码长度不够"}
	PhoneVerifyError           = ErrorCode{"A000151", "手机格式校验失败"}
	IdempotentTokenNullError   = ErrorCode{"A000200", "幂等Token为空"}
	IdempotentTokenDeleteError = ErrorCode{"A000201", "幂等Token已被使用或失效"}
	SearchAmountExceedsLimit   = ErrorCode{"A000300", "查询数据量超过最大限制"}
)

// --- B-class: service errors ---

var (
	ServiceError        = ErrorCode{"B000001", "系统执行出错"}
	ServiceTimeoutError = ErrorCode{"B000100", "系统执行超时"}
)

// --- C-class: remote errors ---

var (
	RemoteError = ErrorCode{"C000001", "调用第三方服务出错"}
)
