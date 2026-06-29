// Package waxerr defines WaxBin's typed errors. Every error surfaced across a
// package boundary carries a stable Code so callers (and the CLI's exit-code
// mapping) can branch on the failure class without string matching.
package waxerr

import (
	"context"
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error class. The CLI maps each Code to a
// fixed process exit code (see cmd/waxbin); the set is part of the public
// contract and only grows.
type Code string

const (
	// CodeInternal is an unexpected failure with no more specific class.
	CodeInternal Code = "internal"
	// CodeInvalid is a validation or usage error (bad config, bad request).
	CodeInvalid Code = "invalid"
	// CodeNotFound is a missing entity (no such pid, path, library).
	CodeNotFound Code = "not_found"
	// CodeConflict is a write-ownership conflict: another process owns the
	// library, or a scoped lease is already held.
	CodeConflict Code = "conflict"
	// CodeLocked is a user-locked field or otherwise immutable state.
	CodeLocked Code = "locked"
	// CodeIO is a filesystem or I/O failure.
	CodeIO Code = "io"
	// CodeUnsupported is a capability that is not available (e.g. a mutating op
	// in read-only mode, or an unsupported platform feature).
	CodeUnsupported Code = "unsupported"
	// CodeCanceled is a deliberate context cancellation or deadline, distinct
	// from an I/O failure.
	CodeCanceled Code = "canceled"
)

// Error is WaxBin's concrete error type. It records the failing operation, a
// human-readable message, the stable Code, and an optional wrapped cause.
type Error struct {
	Code Code   // stable failure class
	Op   string // logical operation, e.g. "store.Open" or "scan.File"
	Msg  string // human-readable detail
	Err  error  // wrapped cause, may be nil
}

func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", e.Op, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	default:
		return e.Op
	}
}

// Unwrap exposes the wrapped cause for errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Err }

// New builds an Error with a message but no wrapped cause.
func New(code Code, op, msg string) *Error {
	return &Error{Code: code, Op: op, Msg: msg}
}

// Wrap builds an Error around an existing cause. Wrap(_, _, nil) returns a true
// nil error, so it is safe to wrap unconditionally and return waxerr.Wrap(...)
// from a func() error. It returns the error interface instead of *Error to avoid
// the typed-nil pitfall where a nil *Error reads as a non-nil error.
func Wrap(code Code, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Op: op, Err: err}
}

// Wrapf is Wrap with a formatted message. Like Wrap it returns a true nil for a
// nil cause.
func Wrapf(code Code, op string, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Op: op, Msg: fmt.Sprintf(format, args...), Err: err}
}

// CodeOf extracts the Code carried by err, walking the wrap chain. It returns
// CodeInternal for non-WaxBin errors and "" for a nil error.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// Is reports whether err carries the given Code anywhere in its wrap chain.
func Is(err error, code Code) bool {
	return CodeOf(err) == code
}

// FromContext classifies err: a context cancellation/deadline becomes
// CodeCanceled, anything else is wrapped under fallback. Returns nil for nil so
// it is safe to wrap unconditionally.
func FromContext(op string, err error, fallback Code) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Wrap(CodeCanceled, op, err)
	}
	return Wrap(fallback, op, err)
}
