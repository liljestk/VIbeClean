package model

import (
	"errors"
	"fmt"
)

// ErrorKind classifies failures so every front end can react the same way,
// whether the backend runs in-process or behind the API.
type ErrorKind int

// Error kinds.
const (
	KindNotFound ErrorKind = iota + 1
	KindInvalidArgument
	KindConflict
	KindUnavailable
)

// Error is a classified failure with a message meant for people.
type Error struct {
	Kind    ErrorKind
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the underlying cause, if any.
func (e *Error) Unwrap() error { return e.Cause }

// Is matches the sentinel of the same kind, so errors.Is(err, ErrNotFound) works.
func (e *Error) Is(target error) bool {
	sentinel, ok := target.(*Error)
	return ok && sentinel.Message == "" && sentinel.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrNotFound        error = &Error{Kind: KindNotFound}
	ErrInvalidArgument error = &Error{Kind: KindInvalidArgument}
	ErrConflict        error = &Error{Kind: KindConflict}
	ErrUnavailable     error = &Error{Kind: KindUnavailable}
)

// NotFound reports something that has no saved data yet.
func NotFound(format string, args ...any) error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf(format, args...)}
}

// InvalidArgument reports a request that can never succeed as written.
func InvalidArgument(format string, args ...any) error {
	return &Error{Kind: KindInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

// Conflict reports a request that lost a race with another one.
func Conflict(format string, args ...any) error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

// Unavailable reports a dependency that is not reachable right now.
func Unavailable(format string, args ...any) error {
	return &Error{Kind: KindUnavailable, Message: fmt.Sprintf(format, args...)}
}

// WithCause attaches the underlying error to a classified error.
func WithCause(err error, cause error) error {
	var classified *Error
	if errors.As(err, &classified) {
		clone := *classified
		clone.Cause = cause
		return &clone
	}
	return err
}
