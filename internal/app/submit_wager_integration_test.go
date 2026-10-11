//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

var policy = app.PendingPolicy{TTL: 15 * time.Minute, MaxAttempts: 10, BackoffBase: time.Second, BackoffCap: 2 * time.Minute}

func (e *env) submitter() *app.SubmitWager {
	return app.NewSubmitWager(e.tx, app.SystemClock{}, app.UUIDv7{}, policy)
}

func (e *env) wallet(balance string) *wallet.Wallet {
	e.t.Helper()
	w, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: uuid.New(), InitialBalance: brl(e.t, balance)})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) request(w *wallet.Wallet, kind wager.Kind, amount, ext, ref string) wager.Request {
	return wager.Request{
		ProviderID: "provider-a", ExternalTransactionID: ext, PlayerID: w.PlayerID(), WalletID: w.ID(),
		RoundID: "round-1", GameID: "game-1", Kind: kind, Money: brl(e.t, amount),
		ReferenceExternalTransactionID: ref,
	}
}

func (e *env) submit(r wager.Request, key string) (app.SubmitResult, error) {
	return e.submitter().Execute(ctx, app.SubmitCommand{Request: r, IdempotencyKey: key, CorrelationID: key})
}

func (e *env) mustSubmit(r wager.Request, key string) app.SubmitResult {
	e.t.Helper()
	res, err := e.submit(r, key)
	if err != nil {
		e.t.Fatalf("submit %s %s: %v", r.Kind, r.ExternalTransactionID, err)
	}
	return res
}

func (e *env) balance(w *wallet.Wallet) (minor int64, version int64) {
	e.t.Helper()
	if err := e.check.QueryRow(ctx, `SELECT balance_minor, version FROM wallets WHERE id = $1`, w.ID()).Scan(&minor, &version); err != nil {
		e.t.Fatal(err)
	}
	return
}

// assertReconciled checks the final invariant of §13: stored balance equals
// Σ credits − Σ debits, and the last entry's version equals the wallet's.
func (e *env) assertReconciled(w *wallet.Wallet) {
	e.t.Helper()
	var stored, net int64
	err := e.check.QueryRow(ctx, `SELECT w.balance_minor,
		COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END)
		          FROM wallet_ledger_entries l WHERE l.wallet_id = w.id), 0)::bigint
		FROM wallets w WHERE w.id = $1`, w.ID()).Scan(&stored, &net)
	if err != nil {
		e.t.Fatal(err)
	}
	if stored != net {
		e.t.Fatalf("wallet %s: stored %d != ledger %d", w.ID(), stored, net)
	}
}

func expectStatus(t *testing.T, res app.SubmitResult, status wager.Status, replay bool) {
	t.Helper()
	if res.Transaction == nil || res.Transaction.Status() != status || res.Replay != replay {
		var got wager.Status
		if res.Transaction != nil {
			got = res.Transaction.Status()
		}
		t.Fatalf("result = %s replay=%v, want %s replay=%v", got, res.Replay, status, replay)
	}
}

func TestSubmitBetCommitsEverythingTogether(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("1000.00")
	res := e.mustSubmit(e.request(w, wager.KindBet, "25.00", "tx-1", ""), "provider-a:tx-1")
	expectStatus(t, res, wager.StatusProcessed, false)
	if res.Transaction.ResultBalance() != brl(t, "975.00") || res.Transaction.ResultWalletVersion() != 2 {
		t.Fatalf("result balance %s v%d", res.Transaction.ResultBalance(), res.Transaction.ResultWalletVersion())
	}
	if b, v := e.balance(w); b != 97500 || v != 2 {
		t.Fatalf("stored %d v%d", b, v)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, w.ID()); n != 2 {
		t.Fatalf("entries = %d, want opening + debit", n)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type IN ('WagerTransactionProcessed', 'WalletBalanceChanged')`); n != 4 {
		t.Fatalf("outbox = %d, want 2 for the opening + 2 for the bet", n)
	}
	e.assertReconciled(w)
}

func TestRejectionIsPersistedAndReplayed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	r := e.request(w, wager.KindBet, "200.00", "tx-1", "")
	res := e.mustSubmit(r, "k1")
	expectStatus(t, res, wager.StatusRejected, false)
	if res.Transaction.FailureCode() != wager.CodeInsufficientFunds {
		t.Fatalf("code = %s", res.Transaction.FailureCode())
	}
	again := e.mustSubmit(r, "k1")
	expectStatus(t, again, wager.StatusRejected, true)
	if again.Transaction.ID() != res.Transaction.ID() {
		t.Fatal("the replay must return the stored transaction")
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionRejected'`); n != 1 {
		t.Fatalf("rejected events = %d, want 1 (the replay emits nothing)", n)
	}
	if b, v := e.balance(w); b != 10000 || v != 1 {
		t.Fatalf("a rejection moved money: %d v%d", b, v)
	}
}

// TestReplayReturnsTheOriginalBalance (§9): not the current one.
func TestReplayReturnsTheOriginalBalance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	first := e.request(w, wager.KindBet, "10.00", "tx-1", "")
	e.mustSubmit(first, "k1")
	e.mustSubmit(e.request(w, wager.KindBet, "20.00", "tx-2", ""), "k2")
	replay := e.mustSubmit(first, "k1")
	expectStatus(t, replay, wager.StatusProcessed, true)
	if replay.Transaction.ResultBalance() != brl(t, "90.00") {
		t.Fatalf("replay balance = %s, want the original 90.00", replay.Transaction.ResultBalance())
	}
}

func TestIdempotencyConflicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	e.mustSubmit(e.request(w, wager.KindBet, "10.00", "tx-1", ""), "k1")

	_, err := e.submit(e.request(w, wager.KindBet, "11.00", "tx-1", ""), "k1")
	if !errors.Is(err, app.ErrIdempotencyKeyReused) {
		t.Fatalf("same key, other body: err = %v", err)
	}
	_, err = e.submit(e.request(w, wager.KindBet, "10.00", "tx-1", ""), "k2")
	if !errors.Is(err, app.ErrExternalTransactionConflict) {
		t.Fatalf("same external id, other key: err = %v", err)
	}
	if b, _ := e.balance(w); b != 9000 {
		t.Fatalf("a conflict moved money: %d", b)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`); n != 1 {
		t.Fatalf("external transactions = %d, want 1", n)
	}
}

// TestProvidersAreIsolated: keys and external ids are scoped per provider, so
// provider B reusing A's key gets its own operation, never A's result.
func TestProvidersAreIsolated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	a := e.mustSubmit(e.request(w, wager.KindBet, "10.00", "tx-1", ""), "shared-key")
	r := e.request(w, wager.KindBet, "10.00", "tx-1", "")
	r.ProviderID = "provider-b"
	b := e.mustSubmit(r, "shared-key")
	expectStatus(t, b, wager.StatusProcessed, false)
	if a.Transaction.ID() == b.Transaction.ID() {
		t.Fatal("provider B received provider A's transaction")
	}
}

func TestUnknownWalletStoresNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	r := e.request(w, wager.KindBet, "10.00", "tx-1", "")
	r.WalletID = uuid.New()
	if _, err := e.submit(r, "k1"); !errors.Is(err, app.ErrWalletNotFound) {
		t.Fatalf("err = %v, want ErrWalletNotFound", err)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`); n != 0 {
		t.Fatalf("stored %d transactions", n)
	}
}

func TestInvalidRequestStoresNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	r := e.request(w, wager.KindBet, "10.00", "tx-1", "")
	r.Kind = wager.KindOpening
	var inv *wager.InvalidRequestError
	if _, err := e.submit(r, "k1"); !errors.As(err, &inv) || inv.Code != wager.CodeUnsupportedKind {
		t.Fatalf("err = %v, want UNSUPPORTED_KIND", err)
	}
	if _, err := e.submit(e.request(w, wager.KindBet, "10.00", "tx-1", ""), ""); !errors.As(err, &inv) || inv.Code != wager.CodeMissingIdempotencyKey {
		t.Fatalf("err = %v, want MISSING_IDEMPOTENCY_KEY", err)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`); n != 0 {
		t.Fatalf("stored %d transactions", n)
	}
}

func TestRefundBeforeBetWaitsForTheReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	refund := e.request(w, wager.KindRefund, "40.00", "refund-1", "bet-1")
	res := e.mustSubmit(refund, "kr")
	expectStatus(t, res, wager.StatusPendingReference, false)
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE id = $1 AND status = 'PENDING_REFERENCE'
		AND attempts = 1 AND next_attempt_at > now() AND expires_at > now() + interval '14 minutes'`, res.Transaction.ID()); n != 1 {
		t.Fatal("pending schedule not persisted")
	}
	if n := e.count(`SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionPendingReference'`); n != 1 {
		t.Fatalf("pending events = %d", n)
	}
	expectStatus(t, e.mustSubmit(refund, "kr"), wager.StatusPendingReference, true)
}

func TestRefundAfterBetAndSingleReversal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	bet := e.mustSubmit(e.request(w, wager.KindBet, "40.00", "bet-1", ""), "kb")
	refund := e.mustSubmit(e.request(w, wager.KindRefund, "40.00", "refund-1", "bet-1"), "kr")
	expectStatus(t, refund, wager.StatusProcessed, false)
	if refund.Transaction.ReferenceTransactionID() != bet.Transaction.ID() {
		t.Fatal("resolved reference not stored")
	}
	second := e.mustSubmit(e.request(w, wager.KindRollback, "40.00", "rollback-1", "bet-1"), "kro")
	expectStatus(t, second, wager.StatusRejected, false)
	if second.Transaction.FailureCode() != wager.CodeAlreadyReversed {
		t.Fatalf("code = %s, want ALREADY_REVERSED", second.Transaction.FailureCode())
	}
	if b, _ := e.balance(w); b != 10000 {
		t.Fatalf("balance = %d, want 10000", b)
	}
	e.assertReconciled(w)
}

// TestTwoConcurrentBetsOf80On100 is the mandatory test of §8.
func TestTwoConcurrentBetsOf80On100(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	reqs := []wager.Request{
		e.request(w, wager.KindBet, "80.00", "bet-a", ""),
		e.request(w, wager.KindBet, "80.00", "bet-b", ""),
	}
	results := make([]app.SubmitResult, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Go(func() {
			<-start // release both at once to maximise contention
			results[i], errs[i] = e.submit(reqs[i], "key-"+reqs[i].ExternalTransactionID)
		})
	}
	close(start)
	wg.Wait()

	processed, rejected := 0, 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("bet %d: %v", i, errs[i])
		}
		switch res.Transaction.Status() {
		case wager.StatusProcessed:
			processed++
		case wager.StatusRejected:
			rejected++
			if res.Transaction.FailureCode() != wager.CodeInsufficientFunds {
				t.Fatalf("code = %s", res.Transaction.FailureCode())
			}
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed %d, rejected %d; want 1 and 1", processed, rejected)
	}
	assert80Plus80(t, e, w)

	// Resending both changes nothing.
	for _, r := range reqs {
		res := e.mustSubmit(r, "key-"+r.ExternalTransactionID)
		if !res.Replay {
			t.Fatal("resend was not a replay")
		}
	}
	assert80Plus80(t, e, w)
}

func assert80Plus80(t *testing.T, e *env, w *wallet.Wallet) {
	t.Helper()
	if b, v := e.balance(w); b != 2000 || v != 2 {
		t.Fatalf("balance %d v%d, want 2000 v2", b, v)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Fatalf("debit entries = %d, want exactly 1", n)
	}
	e.assertReconciled(w)
}

// TestSameBet50TimesConcurrently: §13.1, one debit, the rest replays. This
// exercises the application's deduplication (the unique index), not SQS's.
func TestSameBet50TimesConcurrently(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	r := e.request(w, wager.KindBet, "1.00", "bet-1", "")
	const n = 50
	results := make([]app.SubmitResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			results[i], errs[i] = e.submit(r, "provider-a:bet-1")
		})
	}
	close(start)
	wg.Wait()
	fresh := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if !res.Replay {
			fresh++
		}
		if res.Transaction.ID() != results[0].Transaction.ID() || res.Transaction.ResultBalance() != brl(t, "99.00") {
			t.Fatalf("attempt %d saw a different outcome", i)
		}
	}
	if fresh != 1 {
		t.Fatalf("%d non-replay results, want exactly 1", fresh)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.ID()); n != 1 {
		t.Fatalf("debit entries = %d", n)
	}
	e.assertReconciled(w)
}

// TestIndependentWalletsDoNotBlockEachOther proves the absence of a global
// lock deterministically: while wallet A is locked by another transaction, a
// bet on wallet B completes, and a bet on A cannot.
func TestIndependentWalletsDoNotBlockEachOther(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := e.wallet("100.00"), e.wallet("100.00")

	holder, err := e.check.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, a.ID()); err != nil {
		t.Fatal(err)
	}

	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := e.submitter().Execute(bctx, app.SubmitCommand{Request: e.request(b, wager.KindBet, "1.00", "b-1", ""), IdempotencyKey: "kb"})
	if err != nil || res.Transaction.Status() != wager.StatusProcessed {
		t.Fatalf("wallet B blocked by wallet A's lock: %v", err)
	}

	actx, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	if _, err := e.submitter().Execute(actx, app.SubmitCommand{Request: e.request(a, wager.KindBet, "1.00", "a-1", ""), IdempotencyKey: "ka"}); err == nil {
		t.Fatal("a bet on the locked wallet A completed: the row lock is not taken")
	}
}

// TestManyWalletsInParallel: N wallets × M bets from N goroutines all succeed
// and every wallet reconciles. The elapsed time is logged for comparison with
// a single wallet.
func TestManyWalletsInParallel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const wallets, bets = 10, 10
	ws := make([]*wallet.Wallet, wallets)
	for i := range ws {
		ws[i] = e.wallet("100.00")
	}
	run := func(set []*wallet.Wallet) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for i, w := range set {
			wg.Go(func() {
				for j := range bets {
					ext := fmt.Sprintf("bet-%d-%d-%s", i, j, uuid.NewString())
					if _, err := e.submit(e.request(w, wager.KindBet, "1.00", ext, ""), ext); err != nil {
						t.Errorf("wallet %d bet %d: %v", i, j, err)
					}
				}
			})
		}
		wg.Wait()
		return time.Since(start)
	}
	one := run(ws[:1])
	all := run(ws)
	t.Logf("%d bets on 1 wallet: %v; %d bets on each of %d wallets in parallel: %v", bets, one, bets, wallets, all)
	for _, w := range ws {
		e.assertReconciled(w)
	}
}
