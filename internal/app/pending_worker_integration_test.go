//go:build integration

package app_test

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// laterClock runs the worker as if d had passed since the submissions.
type laterClock struct{ d time.Duration }

func (c laterClock) Now() time.Time { return time.Now().UTC().Add(c.d) }

func (e *env) worker(after time.Duration) *app.PendingWorker {
	return app.NewPendingWorker(e.tx, laterClock{after}, app.UUIDv7{}, policy, slog.New(slog.DiscardHandler))
}

func (e *env) status(ext string) (status, code string, attempts int) {
	e.t.Helper()
	if err := e.check.QueryRow(ctx, `SELECT status, COALESCE(failure_code, ''), attempts
		FROM wager_transactions WHERE external_transaction_id = $1`, ext).Scan(&status, &code, &attempts); err != nil {
		e.t.Fatal(err)
	}
	return
}

func TestPendingWorkerProcessesARefundOnceItsBetArrives(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	expectStatus(t, e.mustSubmit(e.request(w, wager.KindRefund, "40.00", "refund-1", "bet-1"), "kr"), wager.StatusPendingReference, false)
	expectStatus(t, e.mustSubmit(e.request(w, wager.KindBet, "40.00", "bet-1", ""), "kb"), wager.StatusProcessed, false)

	found, err := e.worker(5 * time.Second).RunOnce(ctx)
	if err != nil || !found {
		t.Fatalf("RunOnce = %v, %v", found, err)
	}
	if st, _, _ := e.status("refund-1"); st != "PROCESSED" {
		t.Fatalf("refund status = %s", st)
	}
	if minor, _ := e.balance(w); minor != 10000 {
		t.Fatalf("balance = %d, want 10000", minor)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type = 'WalletBalanceChanged'`); n != 3 {
		t.Fatalf("balance events = %d, want 3 (opening, bet, refund)", n)
	}
	e.assertReconciled(w)
	if found, _ := e.worker(5 * time.Second).RunOnce(ctx); found {
		t.Fatal("a processed refund was picked up again")
	}
}

func TestPendingWorkerReschedulesThenExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	e.mustSubmit(e.request(w, wager.KindRefund, "40.00", "refund-1", "bet-missing"), "kr")

	if found, err := e.worker(5 * time.Second).RunOnce(ctx); err != nil || !found {
		t.Fatalf("RunOnce = %v, %v", found, err)
	}
	if st, _, attempts := e.status("refund-1"); st != "PENDING_REFERENCE" || attempts != 2 {
		t.Fatalf("after a retry: %s, attempts %d", st, attempts)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE next_attempt_at > now() + interval '5 seconds'`); n != 1 {
		t.Fatal("retry was not rescheduled into the future")
	}

	// After the TTL the operation is rejected for good.
	if found, err := e.worker(16 * time.Minute).RunOnce(ctx); err != nil || !found {
		t.Fatalf("RunOnce = %v, %v", found, err)
	}
	if st, code, _ := e.status("refund-1"); st != "REJECTED" || code != string(wager.CodeReferenceNotFound) {
		t.Fatalf("after TTL: %s %s", st, code)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionRejected'`); n != 1 {
		t.Fatalf("rejection events = %d, want 1", n)
	}
	if minor, _ := e.balance(w); minor != 10000 {
		t.Fatalf("balance changed: %d", minor)
	}
}

func TestPendingWorkersNeverProcessTheSameRowTwice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for i := range 10 {
		w := e.wallet("100.00")
		ext := string(rune('a' + i))
		e.mustSubmit(e.request(w, wager.KindRefund, "40.00", "refund-"+ext, "bet-"+ext), "kr-"+ext)
		e.mustSubmit(e.request(w, wager.KindBet, "40.00", "bet-"+ext, ""), "kb-"+ext)
	}
	var wg sync.WaitGroup
	for range 2 {
		worker := e.worker(5 * time.Second)
		wg.Go(func() {
			for {
				found, err := worker.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if !found {
					return
				}
			}
		})
	}
	wg.Wait()
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE kind = 'REFUND' AND status = 'PROCESSED'`); n != 10 {
		t.Fatalf("processed refunds = %d, want 10", n)
	}
	// One credit per refund: opening, bet and refund entries only.
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries`); n != 30 {
		t.Fatalf("ledger entries = %d, want 30", n)
	}
	if n := e.count(`SELECT count(*) FROM wallets WHERE balance_minor <> 10000`); n != 0 {
		t.Fatalf("%d wallets with a wrong balance", n)
	}
}
