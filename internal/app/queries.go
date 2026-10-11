package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

// 400.
var ErrInvalidPage = errors.New("invalid page request")

// Bounds one page, so a request cannot read a whole wallet history.
const MaxLedgerPage = 200

type LedgerPage struct {
	Records []LedgerRecord
	// The afterSeq of the next page.
	NextAfter int64
	HasMore   bool
}

// Queries are the read-only use cases. Provider scoping is enforced by the
// HTTP layer with the provider from the token.
type Queries struct{ tx TxManager }

func NewQueries(tx TxManager) *Queries { return &Queries{tx: tx} }

func (q *Queries) Wallet(ctx context.Context, id uuid.UUID) (w *wallet.Wallet, err error) {
	err = q.tx.ReadSnapshot(ctx, func(ctx context.Context, r Repos) error {
		w, err = r.Wallets().Get(ctx, id)
		return err
	})
	return w, err
}

func (q *Queries) Transaction(ctx context.Context, id uuid.UUID) (t *wager.Transaction, err error) {
	err = q.tx.ReadSnapshot(ctx, func(ctx context.Context, r Repos) error {
		t, err = r.Wagers().Get(ctx, id)
		return err
	})
	return t, err
}

func (q *Queries) TransactionByExternalID(ctx context.Context, providerID, externalID string) (t *wager.Transaction, err error) {
	err = q.tx.ReadSnapshot(ctx, func(ctx context.Context, r Repos) error {
		t, err = r.Wagers().FindByExternalID(ctx, providerID, externalID)
		if err == nil && t == nil {
			err = ErrTransactionNotFound
		}
		return err
	})
	return t, err
}

// Ledger uses keyset pagination (seq > after), not OFFSET, which shifts as
// rows are added. The wallet lock makes seq order commit order, so no entry
// can appear behind a cursor already handed out.
func (q *Queries) Ledger(ctx context.Context, walletID uuid.UUID, afterSeq int64, limit int) (LedgerPage, error) {
	if limit < 1 || limit > MaxLedgerPage || afterSeq < 0 {
		return LedgerPage{}, fmt.Errorf("%w: limit must be 1..%d and the cursor non-negative", ErrInvalidPage, MaxLedgerPage)
	}
	var page LedgerPage
	err := q.tx.ReadSnapshot(ctx, func(ctx context.Context, r Repos) error {
		if _, err := r.Wallets().Get(ctx, walletID); err != nil {
			return err // 404 for an unknown wallet instead of an empty page
		}
		// One extra row tells whether another page exists, without a COUNT.
		recs, err := r.Ledger().Page(ctx, walletID, afterSeq, limit+1)
		if err != nil {
			return err
		}
		if len(recs) > limit {
			page.HasMore, recs = true, recs[:limit]
		}
		page.Records = recs
		page.NextAfter = afterSeq
		if len(recs) > 0 {
			page.NextAfter = recs[len(recs)-1].Seq
		}
		return nil
	})
	return page, err
}

// Difference = stored − calculated (may be negative).
type Reconciliation struct {
	WalletID       uuid.UUID
	Stored         money.Money
	Calculated     money.Money
	Difference     money.Money
	Consistent     bool
	CheckedEntries int64
}

// Metrics reports what operators must see.
type Metrics interface {
	ReconciliationDivergence()
}

type NopMetrics struct{}

func (NopMetrics) ReconciliationDivergence() {}

type Reconcile struct {
	tx      TxManager
	metrics Metrics
	log     *slog.Logger
}

func NewReconcile(tx TxManager, m Metrics, log *slog.Logger) *Reconcile {
	return &Reconcile{tx: tx, metrics: m, log: log}
}

// Execute reads the wallet and the ledger totals in one REPEATABLE READ
// snapshot; in READ COMMITTED a bet committed between the reads would look
// like a divergence. It never writes: a divergence is reported (response,
// log, metric) and a human decides, keeping the evidence.
func (u *Reconcile) Execute(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	var (
		w      *wallet.Wallet
		totals LedgerTotals
	)
	err := u.tx.ReadSnapshot(ctx, func(ctx context.Context, r Repos) error {
		var err error
		if w, err = r.Wallets().Get(ctx, walletID); err != nil {
			return err
		}
		totals, err = r.Ledger().Totals(ctx, walletID)
		return err
	})
	if err != nil {
		return Reconciliation{}, err
	}
	calculated, err := money.FromMinor(totals.NetMinor, w.Currency())
	if err != nil {
		return Reconciliation{}, err
	}
	diff, err := w.Balance().Sub(calculated)
	if err != nil {
		return Reconciliation{}, err
	}
	rec := Reconciliation{
		WalletID: walletID, Stored: w.Balance(), Calculated: calculated, Difference: diff,
		Consistent: diff.IsZero(), CheckedEntries: totals.Count,
	}
	if !rec.Consistent {
		u.metrics.ReconciliationDivergence()
		// No balances in logs: they are financial data.
		u.log.Warn("wallet reconciliation divergence", "walletId", walletID,
			"difference", diff.String(), "currency", diff.Currency().String(), "checkedEntries", totals.Count)
	}
	return rec, nil
}
