package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

func TestClassifyPostgresErrors(t *testing.T) {
	for _, tt := range []struct {
		code string
		want error
	}{
		{"40001", app.ErrConcurrentConflict}, // serialization_failure
		{"40P01", app.ErrConcurrentConflict}, // deadlock_detected
		{"23505", app.ErrConcurrentConflict}, // unique_violation not mapped by a repository: a race
		{"55P03", app.ErrUnavailable},        // lock_not_available (lock_timeout)
		{"57014", app.ErrUnavailable},        // query_canceled (statement_timeout)
		{"57P01", app.ErrUnavailable},        // admin_shutdown
		{"57P03", app.ErrUnavailable},        // cannot_connect_now
		{"53300", app.ErrUnavailable},        // too_many_connections
		{"08006", app.ErrUnavailable},        // connection_failure
		{"23514", app.ErrInvariantViolation}, // check_violation
		{"23503", app.ErrInvariantViolation}, // foreign_key_violation
		{"23001", app.ErrInvariantViolation}, // restrict_violation (ledger trigger)
		{"42501", app.ErrInvariantViolation}, // insufficient_privilege
	} {
		pgErr := &pgconn.PgError{Code: tt.code}
		got := classify(fmt.Errorf("repo: %w", pgErr))
		if !errors.Is(got, tt.want) {
			t.Errorf("%s: classified as %v, want %v", tt.code, got, tt.want)
		}
		var keep *pgconn.PgError
		if !errors.As(got, &keep) || keep.Code != tt.code {
			t.Errorf("%s: the original PgError must stay in the chain", tt.code)
		}
	}
}

func TestClassifyConnectionErrors(t *testing.T) {
	for name, err := range map[string]error{
		"network":        &net.OpError{Op: "dial", Err: errors.New("connection refused")},
		"unexpected eof": io.ErrUnexpectedEOF,
		"connect":        &pgconn.ConnectError{},
	} {
		if got := classify(err); !errors.Is(got, app.ErrUnavailable) {
			t.Errorf("%s: classified as %v, want ErrUnavailable", name, got)
		}
	}
}

func TestClassifyLeavesOtherErrorsAlone(t *testing.T) {
	if classify(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	// The caller's own cancellation is not a dependency failure.
	if got := classify(context.Canceled); got != context.Canceled {
		t.Fatalf("context.Canceled became %v", got)
	}
	// Application errors returned from inside a transaction pass through.
	if got := classify(app.ErrWalletNotFound); got != app.ErrWalletNotFound {
		t.Fatalf("app error became %v", got)
	}
}
