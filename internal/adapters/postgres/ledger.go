package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
)

type ledgerRepo struct{ q pgx.Tx }

// There is no update or delete, and the application role has no such privilege.
func (r ledgerRepo) Append(ctx context.Context, e ledger.Entry) error {
	_, err := r.q.Exec(ctx, `INSERT INTO wallet_ledger_entries
		(id, wallet_id, transaction_id, direction, currency, amount_minor,
		 balance_before_minor, balance_after_minor, wallet_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()), e.Amount().Currency().String(),
		e.Amount().Minor(), e.Before().Minor(), e.After().Minor(), e.WalletVersion(), e.CreatedAt())
	return err
}

func (r ledgerRepo) Page(ctx context.Context, walletID uuid.UUID, afterSeq int64, limit int) ([]app.LedgerRecord, error) {
	rows, err := r.q.Query(ctx, `SELECT seq, id, wallet_id, transaction_id, direction, currency, amount_minor,
		balance_before_minor, balance_after_minor, wallet_version, created_at
		FROM wallet_ledger_entries WHERE wallet_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, walletID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.LedgerRecord
	for rows.Next() {
		var (
			rec                   app.LedgerRecord
			p                     ledger.Params
			dir, currency         string
			amount, before, after int64
		)
		if err := rows.Scan(&rec.Seq, &p.ID, &p.WalletID, &p.TransactionID, &dir, &currency, &amount,
			&before, &after, &p.WalletVersion, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Direction = ledger.Direction(dir)
		if p.Amount, err = moneyOf(amount, currency); err != nil {
			return nil, err
		}
		if p.Before, err = moneyOf(before, currency); err != nil {
			return nil, err
		}
		if p.After, err = moneyOf(after, currency); err != nil {
			return nil, err
		}
		// Re-validated, so a corrupt row surfaces as an error.
		if rec.Entry, err = ledger.NewEntry(p); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SUM(bigint) is numeric; the ::bigint cast makes overflow an error (22003).
func (r ledgerRepo) Totals(ctx context.Context, walletID uuid.UUID) (app.LedgerTotals, error) {
	var t app.LedgerTotals
	err := r.q.QueryRow(ctx, `SELECT
		COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END), 0)::bigint,
		count(*)
		FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&t.NetMinor, &t.Count)
	return t, err
}
