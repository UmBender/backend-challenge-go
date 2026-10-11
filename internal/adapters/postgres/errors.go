package postgres

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

// classify wraps driver errors the repositories did not map in context with
// the application error that decides what to do. Both stay in the chain:
// errors.Is drives behaviour, errors.As keeps the SQLSTATE for logs.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if kind := sqlstateClass(pgErr.Code); kind != nil {
			return fmt.Errorf("%w: %w", kind, err)
		}
		return err
	}
	var netErr *net.OpError
	var connErr *pgconn.ConnectError
	if errors.As(err, &netErr) || errors.As(err, &connErr) || errors.Is(err, io.ErrUnexpectedEOF) ||
		pgconn.Timeout(err) {
		return fmt.Errorf("%w: %w", app.ErrUnavailable, err)
	}
	// context.Canceled, application errors and unknown errors pass through.
	return err
}

func sqlstateClass(code string) error {
	switch code {
	case "40001", "40P01", "23505":
		// An unmapped unique violation is a race-guard index firing: after a
		// retry the domain sees the committed winner.
		return app.ErrConcurrentConflict
	case "55P03", "57014", "57P01", "57P02", "57P03", "53300":
		// lock/statement timeout, shutdown, too many connections.
		return app.ErrUnavailable
	case "23514", "23503", "23502", "23001", "42501":
		// Constraint, trigger or privilege: a bug, not a retry candidate.
		return app.ErrInvariantViolation
	}
	if len(code) == 5 && code[:2] == "08" {
		return app.ErrUnavailable // class 08: connection exception
	}
	return nil
}
