package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

type walletRepo struct{ q pgx.Tx }

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

func (r walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	s := w.Snapshot()
	_, err := r.q.Exec(ctx, `INSERT INTO wallets (`+walletColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.ID, s.PlayerID, s.Balance.Currency().String(), s.Balance.Minor(), s.Version, s.CreatedAt, s.UpdatedAt)
	if isConstraint(err, "23505", "wallets_player_currency_uq") {
		return app.ErrWalletAlreadyExists
	}
	return err
}

func (r walletRepo) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return r.get(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id)
}

// GetForUpdate uses FOR NO KEY UPDATE, not FOR UPDATE: the wager row inserted
// earlier in the transaction holds a FOR KEY SHARE lock on this wallet through
// its foreign key. FOR UPDATE conflicts with it and two operations on one
// wallet would deadlock; FOR NO KEY UPDATE only conflicts with other writers.
func (r walletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return r.get(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, id)
}

func (r walletRepo) get(ctx context.Context, sql string, id uuid.UUID) (*wallet.Wallet, error) {
	var (
		s        wallet.Snapshot
		currency string
		balance  int64
	)
	err := r.q.QueryRow(ctx, sql, id).Scan(&s.ID, &s.PlayerID, &currency, &balance, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, app.ErrWalletNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.Balance, err = moneyOf(balance, currency); err != nil {
		return nil, err
	}
	return wallet.Rehydrate(s)
}

// The row is already locked, so the version check should always match; it
// turns a future locking mistake into an error instead of a lost update.
func (r walletRepo) UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error {
	s := w.Snapshot()
	tag, err := r.q.Exec(ctx, `UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		WHERE id = $1 AND version = $5`, s.ID, s.Balance.Minor(), s.Version, s.UpdatedAt, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s is no longer at version %d", app.ErrConcurrentConflict, s.ID, expectedVersion)
	}
	return nil
}

func moneyOf(minor int64, currency string) (money.Money, error) {
	cur, err := money.ParseCurrency(currency)
	if err != nil {
		return money.Money{}, err
	}
	return money.FromMinor(minor, cur)
}

func isConstraint(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
