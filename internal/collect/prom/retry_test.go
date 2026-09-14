package prom

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped context canceled", fmt.Errorf("scan: %w", context.Canceled), false},

		{"bad_data (400/422)", &v1.Error{Type: v1.ErrBadData}, false},
		{"client_error (other 4xx)", &v1.Error{Type: v1.ErrClient}, false},
		{"canceled (server-side, mirrors our own cancellation)", &v1.Error{Type: v1.ErrCanceled}, false},
		{"bad_response (contract violation)", &v1.Error{Type: v1.ErrBadResponse}, false},

		{"server_error (5xx)", &v1.Error{Type: v1.ErrServer}, true},
		{"timeout", &v1.Error{Type: v1.ErrTimeout}, true},

		{"plain network error", errors.New("connection reset by peer"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
