package core

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeAuthRequired    ErrorCode = "AUTH_REQUIRED"
	CodeForbidden       ErrorCode = "FORBIDDEN"
	CodeInvalidToken    ErrorCode = "INVALID_TOKEN"
	CodeTokenRevoked    ErrorCode = "TOKEN_REVOKED"
	CodeNodeOffline     ErrorCode = "NODE_OFFLINE"
	CodeRateLimited     ErrorCode = "RATE_LIMITED"
	CodeTooManyRequests ErrorCode = "TOO_MANY_REQUESTS"
	CodeVersionConflict ErrorCode = "VERSION_CONFLICT"
	CodeDBConflict      ErrorCode = "DB_CONFLICT"
	CodeInternal        ErrorCode = "INTERNAL_ERROR"
	CodeValidation      ErrorCode = "VALIDATION_ERROR"
	CodeNotFound        ErrorCode = "NOT_FOUND"
)

type CodedError struct {
	Code    ErrorCode
	Message string
	Err     error
	// Retryable 告知调用平台"稍后重试是否有意义"。
	// DESIGN §3.3 要求节点离线等可恢复故障必须带 retryable=true，
	// 否则 Agent 平台无法程序化区分"该重试"与"该放弃/换参数"，
	// 只能靠匹配错误字符串。
	Retryable bool
}

func (e *CodedError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return string(e.Code)
}

func (e *CodedError) Unwrap() error { return e.Err }

func NewError(code ErrorCode, message string, err error) error {
	return &CodedError{Code: code, Message: message, Err: err}
}

// NewRetryableError 构造可重试的业务错误（如节点离线、并发超限）。
func NewRetryableError(code ErrorCode, message string, err error) error {
	return &CodedError{Code: code, Message: message, Err: err, Retryable: true}
}

func ErrorCodeOf(err error) ErrorCode {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return CodeInternal
}
