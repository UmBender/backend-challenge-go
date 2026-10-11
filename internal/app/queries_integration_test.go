//go:build integration

package app_test

import (
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

func TestQueriesReadWalletsAndTransactions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	q := app.NewQueries(e.tx)
	w := e.wallet("100.00")
	bet := e.mustSubmit(e.request(w, wager.KindBet, "10.00", "tx-1", ""), "k1")

	got, err := q.Wallet(ctx, w.ID())
	if err != nil || got.Balance() != brl(t, "90.00") || got.Version() != 2 {
		t.Fatalf("wallet = %+v, %v", got, err)
	}
	if _, err := q.Wallet(ctx, uuid.New()); !errors.Is(err, app.ErrWalletNotFound) {
		t.Fatalf("unknown wallet: %v", err)
	}
	tx, err := q.Transaction(ctx, bet.Transaction.ID())
	if err != nil || tx.Status() != wager.StatusProcessed {
		t.Fatalf("by id: %v", err)
	}
	if _, err := q.Transaction(ctx, uuid.New()); !errors.Is(err, app.ErrTransactionNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if tx, err := q.TransactionByExternalID(ctx, "provider-a", "tx-1"); err != nil || tx.ID() != bet.Transaction.ID() {
		t.Fatalf("by external id: %v", err)
	}
	// Scoped by provider: B cannot find A's transaction by its external id.
	if _, err := q.TransactionByExternalID(ctx, "provider-b", "tx-1"); !errors.Is(err, app.ErrTransactionNotFound) {
		t.Fatalf("other provider: %v", err)
	}
}

// TestLedgerPagesAreStableWhileEntriesArrive: keyset pagination on seq never
// skips or repeats an entry, even when new entries are appended between pages.
func TestLedgerPagesAreStableWhileEntriesArrive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	q := app.NewQueries(e.tx)
	w := e.wallet("100.00") // entry 1: opening
	for i := range 4 {
		e.mustSubmit(e.request(w, wager.KindBet, "1.00", fmt.Sprintf("b-%d", i), ""), fmt.Sprintf("k-%d", i))
	}
	var versions []int64
	after := int64(0)
	for page := 0; ; page++ {
		p, err := q.Ledger(ctx, w.ID(), after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Records {
			versions = append(versions, r.Entry.WalletVersion())
		}
		if page == 0 {
			// A new entry arrives while the client is between pages.
			e.mustSubmit(e.request(w, wager.KindBet, "1.00", "late", ""), "k-late")
		}
		if !p.HasMore {
			break
		}
		after = p.NextAfter
	}
	want := []int64{1, 2, 3, 4, 5, 6}
	if fmt.Sprint(versions) != fmt.Sprint(want) {
		t.Fatalf("versions across pages = %v, want %v", versions, want)
	}
	if _, err := q.Ledger(ctx, uuid.New(), 0, 10); !errors.Is(err, app.ErrWalletNotFound) {
		t.Fatalf("unknown wallet: %v", err)
	}
	for _, limit := range []int{0, -1, 201} {
		if _, err := q.Ledger(ctx, w.ID(), 0, limit); !errors.Is(err, app.ErrInvalidPage) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	if _, err := q.Ledger(ctx, w.ID(), -1, 10); !errors.Is(err, app.ErrInvalidPage) {
		t.Fatalf("negative cursor: %v", err)
	}
}

type countingMetrics struct{ divergences atomic.Int64 }

func (c *countingMetrics) ReconciliationDivergence() { c.divergences.Add(1) }

func TestReconcileConsistentWallet(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := &countingMetrics{}
	rec := app.NewReconcile(e.tx, m, slog.New(slog.DiscardHandler))
	w := e.wallet("1000.00")
	e.mustSubmit(e.request(w, wager.KindBet, "25.00", "b-1", ""), "k1")
	e.mustSubmit(e.request(w, wager.KindLoss, "0.00", "l-1", ""), "k2")
	got, err := rec.Execute(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || got.Stored != brl(t, "975.00") || got.Calculated != brl(t, "975.00") ||
		!got.Difference.IsZero() || got.CheckedEntries != 2 {
		t.Fatalf("reconciliation = %+v", got)
	}
	if m.divergences.Load() != 0 {
		t.Fatal("no divergence expected")
	}
	if _, err := rec.Execute(ctx, uuid.New()); !errors.Is(err, app.ErrWalletNotFound) {
		t.Fatalf("unknown wallet: %v", err)
	}
}

// TestReconcileReportsDivergenceWithoutFixingIt: tamper with the balance as
// the owner (the version trigger allows it with version + 1, nothing else
// stops the owner), then reconcile.
func TestReconcileReportsDivergenceWithoutFixingIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := &countingMetrics{}
	rec := app.NewReconcile(e.tx, m, slog.New(slog.DiscardHandler))
	w := e.wallet("100.00")
	if _, err := e.check.Exec(ctx, `UPDATE wallets SET balance_minor = balance_minor + 1, version = version + 1 WHERE id = $1`, w.ID()); err != nil {
		t.Fatal(err)
	}
	got, err := rec.Execute(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Consistent || got.Difference != brl(t, "0.01") || got.Stored != brl(t, "100.01") || got.Calculated != brl(t, "100.00") {
		t.Fatalf("reconciliation = %+v", got)
	}
	if m.divergences.Load() != 1 {
		t.Fatalf("divergence metric = %d, want 1", m.divergences.Load())
	}
	if b, _ := e.balance(w); b != 10001 {
		t.Fatalf("reconciliation modified the balance: %d", b)
	}
}
