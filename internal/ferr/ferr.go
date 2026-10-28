// Package ferr holds the error kinds Flatpix uses. Every error that can reach
// an HTTP client is one of these kinds, so the server maps it to a status code
// and a log event in one place.
package ferr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// The sentinels. Test with errors.Is.
var (
	ErrInternal = errors.New("internal error")        // 500, our bug or a broken dependency
	ErrBadInput = errors.New("bad input")             // 400, the request or the source file is wrong
	ErrBusy     = errors.New("busy")                  // 503, no seat or download slot in time
	ErrNotFound = errors.New("not found")             // 404
	ErrTooLarge = errors.New("too large")             // 413, source file or source pixels over the limit
	ErrUpstream = errors.New("upstream fetch failed") // 502, the remote origin failed
)

// Error is a kind plus a message that is safe to show to the client.
type Error struct {
	Kind  error
	Msg   string
	Cause error // logged, never shown
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Msg, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Msg)
}

func (e *Error) Is(target error) bool { return target == e.Kind }
func (e *Error) Unwrap() error        { return e.Cause }

// New makes an error of the given kind.
func New(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// Wrap makes an error of the given kind that remembers its cause.
func Wrap(kind, cause error, format string, a ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, a...), Cause: cause}
}

// Kind returns the sentinel for err. Anything that is not one of ours is
// ErrInternal.
func Kind(err error) error {
	for _, k := range []error{ErrBadInput, ErrNotFound, ErrTooLarge, ErrBusy, ErrUpstream, ErrInternal} {
		if errors.Is(err, k) {
			return k
		}
	}
	return ErrInternal
}

// Status is the HTTP status for err.
func Status(err error) int {
	switch Kind(err) {
	case ErrBadInput:
		return http.StatusBadRequest
	case ErrNotFound:
		return http.StatusNotFound
	case ErrTooLarge:
		return http.StatusRequestEntityTooLarge
	case ErrBusy:
		return http.StatusServiceUnavailable
	case ErrUpstream:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// Event is the log event name for err. There is one per failure type.
func Event(err error) string {
	switch Kind(err) {
	case ErrBadInput:
		return "bad_input"
	case ErrNotFound:
		return "not_found"
	case ErrTooLarge:
		return "too_large"
	case ErrBusy:
		return "busy"
	case ErrUpstream:
		return "upstream_failed"
	}
	return "internal_error"
}

// PublicMessage is the text shown to the client. Internal errors never leak
// their details.
func PublicMessage(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Kind != ErrInternal {
		return e.Msg
	}
	if Kind(err) == ErrInternal {
		return "internal error"
	}
	return err.Error()
}

// ClientGone reports whether err only exists because the client went away.
// reqCtx is the request context. Such an error is neither counted nor logged
// as a failure.
func ClientGone(reqCtx context.Context, err error) bool {
	return err != nil && reqCtx.Err() != nil &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
