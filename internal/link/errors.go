package link

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound covers both "never existed" and "expired or deleted". The
	// API never distinguishes them to a caller.
	ErrNotFound = errors.New("link not found")

	// ErrCodeTaken means the requested custom alias, or a generated code,
	// already exists.
	ErrCodeTaken = errors.New("code already taken")
)

// ValidationError is a caller mistake: it maps to HTTP 400 with a message that
// is safe to return.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s %s", e.Field, e.Msg)
}

func invalid(field, msg string) error {
	return &ValidationError{Field: field, Msg: msg}
}
