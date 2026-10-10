// Package postgres owns the PostgreSQL connection pool. Repositories and the
// transaction manager arrive in later tasks; T0 only opens, checks and closes
// the pool.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

// Module provides *pgxpool.Pool and registers PostgreSQL as a readiness check.
var Module = fx.Module("postgres",
	fx.Provide(
		NewPool,
		health.AsCheck(NewCheck),
	),
)

// NewPool builds the pool without connecting (pgxpool connects lazily), then
// pings on start so a bad URL or unreachable database fails startup, and
// closes on stop. Fx stops hooks in reverse order, so the pool closes only
// after every component that uses it has stopped.
func NewPool(lc fx.Lifecycle, db config.Database, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		// pgx parse errors can echo the connection string; keep the message generic.
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}
	pcfg.MaxConns = db.MaxConns
	// Identifies each replica in pg_stat_activity.
	pcfg.ConnConfig.RuntimeParams["application_name"] = "wallet-" + cfg.InstanceID

	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	log = log.With("component", "postgres")
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("postgres unreachable at startup: %w", err)
			}
			log.Info("postgres pool ready", "maxConns", pcfg.MaxConns)
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close() // waits for acquired connections to be released
			log.Info("postgres pool closed")
			return nil
		},
	})
	return pool, nil
}

// Check reports PostgreSQL readiness.
type Check struct{ pool *pgxpool.Pool }

// NewCheck is the Fx constructor for the readiness check.
func NewCheck(pool *pgxpool.Pool) *Check { return &Check{pool: pool} }

// Name implements health.Check.
func (c *Check) Name() string { return "postgres" }

// Check implements health.Check with a round trip to the server.
func (c *Check) Check(ctx context.Context) error { return c.pool.Ping(ctx) }
