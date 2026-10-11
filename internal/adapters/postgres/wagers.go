package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

type wagerRepo struct{ q pgx.Tx }

const wagerColumns = `id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id, failure_code,
	result_balance_minor, result_wallet_version, attempts, next_attempt_at, expires_at,
	created_at, updated_at, processed_at`

const wagerInsert = `INSERT INTO wager_transactions (` + wagerColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)`

func (r wagerRepo) Insert(ctx context.Context, t *wager.Transaction) error {
	_, err := r.q.Exec(ctx, wagerInsert, wagerArgs(t.Snapshot())...)
	return mapWalletFK(err)
}

// InsertIfAbsent is the heart of idempotency. ON CONFLICT DO NOTHING covers
// every unique index; if a concurrent transaction holds a conflicting row,
// PostgreSQL waits for it, so exactly one transaction owns the operation,
// without a racy SELECT-then-INSERT. It does not abort the transaction, so
// the caller can read the existing row next.
func (r wagerRepo) InsertIfAbsent(ctx context.Context, t *wager.Transaction) (bool, error) {
	tag, err := r.q.Exec(ctx, wagerInsert+` ON CONFLICT DO NOTHING`, wagerArgs(t.Snapshot())...)
	if err != nil {
		return false, mapWalletFK(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r wagerRepo) Update(ctx context.Context, t *wager.Transaction) error {
	a := wagerArgs(t.Snapshot())
	tag, err := r.q.Exec(ctx, `UPDATE wager_transactions SET status = $2, reference_transaction_id = $3,
		failure_code = $4, result_balance_minor = $5, result_wallet_version = $6, attempts = $7,
		next_attempt_at = $8, expires_at = $9, updated_at = $10, processed_at = $11
		WHERE id = $1`, a[0], a[3], a[15], a[16], a[17], a[18], a[19], a[20], a[21], a[23], a[24])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("update wager transaction %s: %w", t.ID(), app.ErrTransactionNotFound)
	}
	return nil
}

func (r wagerRepo) Get(ctx context.Context, id uuid.UUID) (*wager.Transaction, error) {
	t, err := scanWager(r.q.QueryRow(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, app.ErrTransactionNotFound
	}
	return t, err
}

// Always filtered by provider: one provider cannot reach another's row.
func (r wagerRepo) FindByKey(ctx context.Context, providerID, key string) (*wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+` FROM wager_transactions
		WHERE origin = 'EXTERNAL' AND provider_id = $1 AND idempotency_key = $2`, providerID, key)
}

func (r wagerRepo) FindByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error) {
	return r.findOne(ctx, `SELECT `+wagerColumns+` FROM wager_transactions
		WHERE origin = 'EXTERNAL' AND provider_id = $1 AND external_transaction_id = $2`, providerID, externalID)
}

func (r wagerRepo) findOne(ctx context.Context, sql string, args ...any) (*wager.Transaction, error) {
	t, err := scanWager(r.q.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (r wagerRepo) HasProcessedReversal(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wager_transactions
		WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK'))`, id).Scan(&exists)
	return exists, err
}

// The database answers "does this wallet exist", with no window between
// check and insert.
func mapWalletFK(err error) error {
	if isConstraint(err, "23503", "wager_transactions_wallet_id_fkey") {
		return app.ErrWalletNotFound
	}
	return err
}

// Empty strings, nil UUID, zero time and Money{} become NULL ("not applicable").
func wagerArgs(s wager.Snapshot) []any {
	var resultBalance, resultVersion *int64
	if s.ResultBalance.IsValid() {
		b, v := s.ResultBalance.Minor(), s.ResultWalletVersion
		resultBalance, resultVersion = &b, &v
	}
	var ref *uuid.UUID
	if s.ReferenceTransactionID != uuid.Nil {
		ref = &s.ReferenceTransactionID
	}
	return []any{
		s.ID, string(s.Origin), string(s.Kind), string(s.Status), s.WalletID, s.PlayerID,
		s.Money.Currency().String(), s.Money.Minor(),
		textOrNil(s.ProviderID), textOrNil(s.ExternalTransactionID), textOrNil(s.IdempotencyKey),
		bytesOrNil(s.PayloadHash), textOrNil(s.RoundID), textOrNil(s.GameID),
		textOrNil(s.ReferenceExternalTransactionID), ref, textOrNil(string(s.FailureCode)),
		resultBalance, resultVersion, s.Attempts, timeOrNil(s.NextAttemptAt), timeOrNil(s.ExpiresAt),
		s.CreatedAt, s.UpdatedAt, timeOrNil(s.ProcessedAt),
	}
}

func textOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func bytesOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func scanWager(row pgx.Row) (*wager.Transaction, error) {
	var (
		s                                                     wager.Snapshot
		origin, kind, status, currency                        string
		amount                                                int64
		provider, external, key, round, game, refExt, failure *string
		hash                                                  []byte
		ref                                                   *uuid.UUID
		resultBalance, resultVersion                          *int64
		next, expires, processed                              *time.Time
	)
	err := row.Scan(&s.ID, &origin, &kind, &status, &s.WalletID, &s.PlayerID, &currency, &amount,
		&provider, &external, &key, &hash, &round, &game, &refExt, &ref, &failure,
		&resultBalance, &resultVersion, &s.Attempts, &next, &expires, &s.CreatedAt, &s.UpdatedAt, &processed)
	if err != nil {
		return nil, err
	}
	s.Origin, s.Kind, s.Status = wager.Origin(origin), wager.Kind(kind), wager.Status(status)
	if s.Money, err = moneyOf(amount, currency); err != nil {
		return nil, err
	}
	s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey = deref(provider), deref(external), deref(key)
	s.RoundID, s.GameID, s.ReferenceExternalTransactionID = deref(round), deref(game), deref(refExt)
	s.FailureCode = wager.FailureCode(deref(failure))
	s.PayloadHash = hash
	if ref != nil {
		s.ReferenceTransactionID = *ref
	}
	if resultBalance != nil {
		if s.ResultBalance, err = moneyOf(*resultBalance, currency); err != nil {
			return nil, err
		}
		s.ResultWalletVersion = *resultVersion
	}
	for dst, src := range map[*time.Time]*time.Time{&s.NextAttemptAt: next, &s.ExpiresAt: expires, &s.ProcessedAt: processed} {
		if src != nil {
			*dst = src.UTC()
		}
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return wager.Rehydrate(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
