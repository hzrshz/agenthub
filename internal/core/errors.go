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

func ErrorCodeOf(err error) ErrorCode {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return CodeInternal
}
