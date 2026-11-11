package domain

import (
	"errors"
	"fmt"
)

// ErrorKind maps domain failures to transport-neutral categories.
type ErrorKind string

const (
	KindInvalid  ErrorKind = "invalid"
	KindConflict ErrorKind = "conflict"
	KindNotFound ErrorKind = "not_found"
	KindTooLarge ErrorKind = "too_large"
	KindUnready  ErrorKind = "unready"
	KindGone     ErrorKind = "gone"
	KindInternal ErrorKind = "internal"
)

// ErrorDetail is the stable machine-readable portion of an AppError.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// AppError carries a client-safe message and a stable code.
type AppError struct {
	Kind   ErrorKind
	Detail ErrorDetail
	Cause  error
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail.Field != "" {
		return fmt.Sprintf("%s (%s): %s", e.Detail.Code, e.Detail.Field, e.Detail.Message)
	}
	return fmt.Sprintf("%s: %s", e.Detail.Code, e.Detail.Message)
}

func (e *AppError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func E(kind ErrorKind, code, field, message string) *AppError {
	return &AppError{
		Kind: kind,
		Detail: ErrorDetail{
			Code:    code,
			Message: message,
			Field:   field,
		},
	}
}

func Wrap(kind ErrorKind, code, field, message string, cause error) *AppError {
	err := E(kind, code, field, message)
	err.Cause = cause
	return err
}

func AsAppError(err error) *AppError {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return Wrap(KindInternal, "internal_error", "", "the request could not be completed", err)
}
