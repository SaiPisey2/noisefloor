package prom

import (
	"context"
	"errors"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// IsRetryable reports whether err is worth retrying: a transient failure that
// may succeed on a later attempt, as opposed to one that will fail
// identically every time.
//
// Context cancellation and deadline expiry are never retryable -- the caller
// asked to stop, and retrying would ignore that.
//
// A *v1.Error means the request reached Prometheus and it told us something
// specific: ErrServer (a 5xx) and ErrTimeout (the query itself timed out
// server-side) are transient and worth another attempt. Everything else the
// API returns is not: ErrBadData is a 400/422, meaning the query or
// parameters are wrong and will be wrong again; ErrClient covers other 4xx
// (401/403/404, ...), which describe a request that is not going to become
// valid by waiting; ErrCanceled means the server's own query execution was
// canceled, almost always because OUR context already was, which the caller
// handles separately; ErrBadResponse means the response did not match the
// API's contract at all, which a retry cannot fix.
//
// Anything else -- no *v1.Error at all -- means the failure never reached
// Prometheus's API layer: a dial failure, a connection reset, a transport
// timeout, a DNS lookup failure. Those are exactly the transient failures a
// retry exists for.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var perr *v1.Error
	if errors.As(err, &perr) {
		switch perr.Type {
		case v1.ErrServer, v1.ErrTimeout:
			return true
		default:
			return false
		}
	}
	return true
}
