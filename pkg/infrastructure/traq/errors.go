package traq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Keep status and operation separate from response bodies and transport errors.
type lookupError struct {
	operation string
	status    int
	cause     error
}

func (e *lookupError) Error() string {
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(e.cause, reason) {
			return fmt.Sprintf("%s: %s", e.operation, reason)
		}
	}
	if e.status != 0 {
		return fmt.Sprintf("%s failed (HTTP %d)", e.operation, e.status)
	}
	return e.operation + " failed"
}

func (e *lookupError) Unwrap() error { return e.cause }

func apiError(operation string, response *http.Response, err error) error {
	failure := &lookupError{operation: operation, cause: err}
	if response != nil {
		failure.status = response.StatusCode
	}
	return failure
}

func (a *adapter) warnLookup(ctx context.Context, operation string, err error, ids ...any) {
	if ctx.Err() != nil {
		return
	}
	status := 0
	var failure *lookupError
	if errors.As(err, &failure) {
		operation, status = failure.operation, failure.status
	}
	attrs := []any{"adapter", a.name, "operation", operation, "http_status", status}
	attrs = append(attrs, ids...)
	slog.WarnContext(ctx, "traQ input skipped", attrs...)
}
