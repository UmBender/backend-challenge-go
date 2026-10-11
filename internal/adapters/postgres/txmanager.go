package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

// TxManager is the only place that begins, commits and rolls back; the
// repositories only receive the pgx.Tx.
type TxManager struct{ pool *pgxpool.Pool }

func NewTxManager(pool *pgxpool.Pool) *TxManager { return &TxManager{pool: pool} }

var _ app.TxManager = (*TxManager)(nil)

// READ COMMITTED on purpose: under the wallet row lock each statement sees
// the latest commit, so the second of two concurrent bets reads the new
// balance and is rejected without a retry. REPEATABLE READ or SERIALIZABLE
// would abort it with 40001 instead.
func (m *TxManager) WithinTx(ctx context.Context, fn func(context.Context, app.Repos) error) error {
	return m.run(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

func (m *TxManager) ReadSnapshot(ctx context.Context, fn func(context.Context, app.Repos) error) error {
	return m.run(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (m *TxManager) run(ctx context.Context, opts pgx.TxOptions, fn func(context.Context, app.Repos) error) (err error) {
	tx, err := m.pool.BeginTx(ctx, opts)
	if err != nil {
		return classify(fmt.Errorf("begin: %w", err))
	}
	defer func() {
		if err != nil {
			// Roll back even if ctx was cancelled, to keep the pooled connection.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(ctx, repos{q: tx}); err != nil {
		return classify(err)
	}
	if err = tx.Commit(ctx); err != nil {
		// A failed COMMIT may have an unknown outcome; the client's retry with the
		// same key either replays or performs the operation once.
		return classify(fmt.Errorf("commit: %w", err))
	}
	return nil
}

type repos struct{ q pgx.Tx }

func (r repos) Wallets() app.WalletRepo { return walletRepo{q: r.q} }
func (r repos) Wagers() app.WagerRepo   { return wagerRepo{q: r.q} }
func (r repos) Ledger() app.LedgerRepo  { return ledgerRepo{q: r.q} }
func (r repos) Outbox() app.OutboxRepo  { return outboxRepo{q: r.q} }
func (r repos) Inbox() app.InboxRepo    { return inboxRepo{q: r.q} }
