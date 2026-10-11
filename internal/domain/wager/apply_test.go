package wager

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

// fixture is one wallet and helpers to send operations to it.
type fixture struct {
	t      *testing.T
	w      *wallet.Wallet
	now    time.Time
	nextID int
}

func newFixture(t *testing.T, balance string) *fixture {
	t.Helper()
	w, err := wallet.New(uuid.New(), uuid.New(), brl(t, balance), t0)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, w: w, now: t0}
}

func (f *fixture) request(kind Kind, amount, ref string) Request {
	f.nextID++
	return Request{
		ProviderID: "provider-a", ExternalTransactionID: "ext-" + string(rune('a'+f.nextID)),
		PlayerID: f.w.PlayerID(), WalletID: f.w.ID(), RoundID: "round-1", GameID: "game-1",
		Kind: kind, Money: brl(f.t, amount), ReferenceExternalTransactionID: ref,
	}
}

func (f *fixture) policy() RetryPolicy {
	return RetryPolicy{NextAttemptAt: f.now.Add(time.Second), TTL: 15 * time.Minute, MaxAttempts: 10}
}

// apply builds a new transaction from r and applies it against the wallet.
func (f *fixture) apply(r Request, ref Reference) (*Transaction, Decision) {
	f.t.Helper()
	tx := newExternal(f.t, r)
	return tx, f.reapply(tx, ref)
}

func (f *fixture) reapply(tx *Transaction, ref Reference) Decision {
	f.t.Helper()
	f.now = f.now.Add(time.Second)
	d, err := tx.Apply(ApplyInput{Wallet: f.w, Reference: ref, EntryID: uuid.New(), Retry: f.policy(), Now: f.now})
	if err != nil {
		f.t.Fatalf("Apply(%s): %v", tx.Kind(), err)
	}
	return d
}

// processed applies r and fails the test unless it was processed.
func (f *fixture) processed(r Request, ref Reference) *Transaction {
	f.t.Helper()
	tx, d := f.apply(r, ref)
	if d.Outcome != OutcomeProcessed {
		f.t.Fatalf("%s: outcome %s %v, want processed", r.Kind, d.Outcome, d.Rejection)
	}
	return tx
}

func (f *fixture) bet(amount string) *Transaction {
	return f.processed(f.request(KindBet, amount, ""), Reference{})
}

func expectRejected(t *testing.T, d Decision, code FailureCode) {
	t.Helper()
	if d.Outcome != OutcomeRejected || d.Rejection == nil || d.Rejection.Code != code {
		t.Fatalf("decision = %s %v, want rejected %s", d.Outcome, d.Rejection, code)
	}
	if d.Entry != nil {
		t.Fatal("a rejection must not produce a ledger entry")
	}
	if len(d.Events) != 1 {
		t.Fatalf("events = %d, want exactly WagerTransactionRejected", len(d.Events))
	}
	if e, ok := d.Events[0].(event.WagerTransactionRejected); !ok || e.FailureCode != string(code) {
		t.Fatalf("event = %#v", d.Events[0])
	}
}

func expectBalance(t *testing.T, f *fixture, amount string, version int64) {
	t.Helper()
	if f.w.Balance() != brl(t, amount) || f.w.Version() != version {
		t.Fatalf("wallet = %s v%d, want %s v%d", f.w.Balance(), f.w.Version(), amount, version)
	}
}

func TestBetDebits(t *testing.T) {
	f := newFixture(t, "100.00")
	tx, d := f.apply(f.request(KindBet, "25.00", ""), Reference{})
	if d.Outcome != OutcomeProcessed || d.Entry == nil || d.Entry.Direction() != ledger.Debit || d.Entry.After() != brl(t, "75.00") {
		t.Fatalf("decision = %+v", d)
	}
	expectBalance(t, f, "75.00", 2)
	if tx.Status() != StatusProcessed || tx.ResultBalance() != brl(t, "75.00") || tx.ResultWalletVersion() != 2 {
		t.Fatalf("result = %s %s v%d", tx.Status(), tx.ResultBalance(), tx.ResultWalletVersion())
	}
	if len(d.Events) != 2 {
		t.Fatalf("events = %d, want processed + balance changed", len(d.Events))
	}
	if _, ok := d.Events[0].(event.WagerTransactionProcessed); !ok {
		t.Fatalf("first event %#v", d.Events[0])
	}
	if b, ok := d.Events[1].(event.WalletBalanceChanged); !ok || b.BalanceBefore != brl(t, "100.00") || b.WalletVersion != 2 {
		t.Fatalf("second event %#v", d.Events[1])
	}
}

// TestTwoBetsOf80On100 is the domain half of the mandatory scenario (§8).
func TestTwoBetsOf80On100(t *testing.T) {
	f := newFixture(t, "100.00")
	f.bet("80.00")
	tx, d := f.apply(f.request(KindBet, "80.00", ""), Reference{})
	expectRejected(t, d, CodeInsufficientFunds)
	if tx.Status() != StatusRejected || tx.FailureCode() != CodeInsufficientFunds {
		t.Fatalf("tx = %s %s", tx.Status(), tx.FailureCode())
	}
	expectBalance(t, f, "20.00", 2)
}

func TestWinCreditsWithOrWithoutReference(t *testing.T) {
	f := newFixture(t, "100.00")
	f.processed(f.request(KindWin, "50.00", ""), Reference{})
	expectBalance(t, f, "150.00", 2)

	bet := f.bet("10.00")
	win := f.processed(f.request(KindWin, "30.00", bet.ExternalTransactionID()), Reference{Tx: bet})
	expectBalance(t, f, "170.00", 4)
	if win.ReferenceTransactionID() != bet.ID() {
		t.Fatal("the resolved reference must be recorded")
	}
}

func TestWinReferenceRules(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.bet("10.00")
	otherRound := f.request(KindBet, "10.00", "")
	otherRound.RoundID = "round-2"
	betRound2 := f.processed(otherRound, Reference{})
	win := f.processed(f.request(KindWin, "5.00", ""), Reference{})

	_, d := f.apply(f.request(KindWin, "5.00", "x"), Reference{Tx: betRound2})
	expectRejected(t, d, CodeReferenceMismatch)
	_, d = f.apply(f.request(KindWin, "5.00", "x"), Reference{Tx: win})
	expectRejected(t, d, CodeReferenceKindInvalid)
	// Decision: a WIN against a bet that was already reversed is rejected,
	// so a prize is never credited for a bet that no longer stands.
	_, d = f.apply(f.request(KindWin, "5.00", "x"), Reference{Tx: bet, AlreadyReversed: true})
	expectRejected(t, d, CodeAlreadyReversed)
	// Decision: a WIN whose bet has not arrived waits, like a reversal.
	_, d = f.apply(f.request(KindWin, "5.00", "missing"), Reference{})
	if d.Outcome != OutcomeAwaitingReference {
		t.Fatalf("outcome = %s, want awaiting reference", d.Outcome)
	}
}

func TestLossChangesNothingButIsProcessed(t *testing.T) {
	f := newFixture(t, "100.00")
	tx, d := f.apply(f.request(KindLoss, "0.00", ""), Reference{})
	if d.Outcome != OutcomeProcessed || d.Entry != nil {
		t.Fatalf("decision = %+v", d)
	}
	expectBalance(t, f, "100.00", 1)
	if tx.ResultBalance() != brl(t, "100.00") || tx.ResultWalletVersion() != 1 {
		t.Fatal("LOSS result must be the current balance and version")
	}
	if len(d.Events) != 1 {
		t.Fatalf("events = %d, want only WagerTransactionProcessed", len(d.Events))
	}
	if _, ok := d.Events[0].(event.WagerTransactionProcessed); !ok {
		t.Fatalf("event %#v", d.Events[0])
	}
}

func TestEveryKindChecksPlayerAndCurrency(t *testing.T) {
	for _, kind := range []Kind{KindBet, KindWin, KindLoss, KindRefund, KindRollback} {
		amount, ref := "10.00", ""
		if kind == KindLoss {
			amount = "0.00"
		}
		if kind.isReversal() {
			ref = "bet"
		}
		f := newFixture(t, "100.00")
		r := f.request(kind, amount, ref)
		r.PlayerID = uuid.New()
		_, d := f.apply(r, Reference{})
		expectRejected(t, d, CodeWalletPlayerMismatch)

		usdWallet, _ := wallet.New(uuid.New(), uuid.New(), mustUSD(t, "100.00"), t0)
		f = &fixture{t: t, w: usdWallet, now: t0}
		_, d = f.apply(f.request(kind, amount, ref), Reference{})
		expectRejected(t, d, CodeCurrencyMismatch)
		if f.w.Version() != 1 {
			t.Fatalf("%s: rejection changed the wallet", kind)
		}
	}
}

func TestRefund(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.bet("40.00")
	refund := f.processed(f.request(KindRefund, "40.00", bet.ExternalTransactionID()), Reference{Tx: bet})
	expectBalance(t, f, "100.00", 3)
	if refund.ReferenceTransactionID() != bet.ID() {
		t.Fatal("reference not recorded")
	}
}

func TestReversalReferenceRules(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.bet("40.00")
	win := f.processed(f.request(KindWin, "10.00", ""), Reference{})
	loss := f.processed(f.request(KindLoss, "0.00", ""), Reference{})
	rejected, _ := f.apply(f.request(KindBet, "1000.00", ""), Reference{})
	pending := newExternal(t, f.request(KindBet, "1.00", ""))

	for name, tt := range map[string]struct {
		kind   Kind
		amount string
		ref    Reference
		code   FailureCode
	}{
		"refund of a win":         {KindRefund, "10.00", Reference{Tx: win}, CodeReferenceKindInvalid},
		"refund of a loss":        {KindRefund, "0.01", Reference{Tx: loss}, CodeReferenceKindInvalid},
		"partial refund":          {KindRefund, "39.99", Reference{Tx: bet}, CodeReferenceAmountMismatch},
		"refund twice":            {KindRefund, "40.00", Reference{Tx: bet, AlreadyReversed: true}, CodeAlreadyReversed},
		"refund of rejected bet":  {KindRefund, "1000.00", Reference{Tx: rejected}, CodeReferenceNotProcessed},
		"rollback of a loss":      {KindRollback, "0.01", Reference{Tx: loss}, CodeReferenceKindInvalid},
		"partial rollback":        {KindRollback, "1.00", Reference{Tx: bet}, CodeReferenceAmountMismatch},
		"rollback after reversal": {KindRollback, "40.00", Reference{Tx: bet, AlreadyReversed: true}, CodeAlreadyReversed},
		"rollback of rejected":    {KindRollback, "1000.00", Reference{Tx: rejected}, CodeReferenceNotProcessed},
	} {
		v := f.w.Version()
		_, d := f.apply(f.request(tt.kind, tt.amount, "ref"), tt.ref)
		t.Run(name, func(t *testing.T) { expectRejected(t, d, tt.code) })
		if f.w.Version() != v {
			t.Fatalf("%s changed the wallet", name)
		}
	}

	// A reference that exists but is still pending may still succeed: wait.
	_, d := f.apply(f.request(KindRefund, "1.00", "ref"), Reference{Tx: pending})
	if d.Outcome != OutcomeAwaitingReference {
		t.Fatalf("outcome = %s, want awaiting reference", d.Outcome)
	}
}

func TestReferenceMustMatchTheOperation(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.bet("10.00")
	for name, change := range map[string]func(*Request){
		"round":    func(r *Request) { r.RoundID = "round-2" },
		"provider": func(r *Request) { r.ProviderID = "provider-b" },
	} {
		r := f.request(KindRefund, "10.00", bet.ExternalTransactionID())
		change(&r)
		_, d := f.apply(r, Reference{Tx: bet})
		t.Run(name, func(t *testing.T) { expectRejected(t, d, CodeReferenceMismatch) })
	}
	// Same provider, same player, but the reference belongs to another wallet.
	other := newFixture(t, "100.00")
	otherReq := other.request(KindBet, "10.00", "")
	otherReq.PlayerID = f.w.PlayerID()
	otherBet := newExternal(t, otherReq)
	_ = otherBet.markProcessed(uuid.Nil, brl(t, "90.00"), 2, t0)
	_, d := f.apply(f.request(KindRefund, "10.00", "x"), Reference{Tx: otherBet})
	expectRejected(t, d, CodeReferenceMismatch)
}

func TestRollbackDirections(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.bet("40.00") // 60
	f.processed(f.request(KindRollback, "40.00", "b"), Reference{Tx: bet})
	expectBalance(t, f, "100.00", 3)

	win := f.processed(f.request(KindWin, "30.00", ""), Reference{}) // 130
	_, d := f.apply(f.request(KindRollback, "30.00", "w"), Reference{Tx: win})
	if d.Outcome != OutcomeProcessed || d.Entry.Direction() != ledger.Debit {
		t.Fatalf("rollback of a win must debit: %+v", d)
	}
	expectBalance(t, f, "100.00", 5)

	bet2 := f.bet("20.00")                                                           // 80
	refund := f.processed(f.request(KindRefund, "20.00", "b2"), Reference{Tx: bet2}) // 100
	_, d = f.apply(f.request(KindRollback, "20.00", "r"), Reference{Tx: refund})     // 80
	if d.Outcome != OutcomeProcessed || d.Entry.Direction() != ledger.Debit {
		t.Fatalf("rollback of a refund must debit: %+v", d)
	}
	expectBalance(t, f, "80.00", 8)

	rollback := f.processed(f.request(KindRollback, "80.00", "x"), Reference{Tx: f.bet("80.00")}) // 0 -> 80
	_, d = f.apply(f.request(KindRollback, "80.00", "rb"), Reference{Tx: rollback})
	expectRejected(t, d, CodeReferenceKindInvalid)
}

// TestReversalInsufficientFunds: a rollback that would overdraw the wallet is
// rejected with a code distinct from a bet's INSUFFICIENT_FUNDS (§7).
func TestReversalInsufficientFunds(t *testing.T) {
	f := newFixture(t, "0.00")
	win := f.processed(f.request(KindWin, "50.00", ""), Reference{})
	f.bet("30.00") // 20 left, the win of 50 cannot be taken back
	_, d := f.apply(f.request(KindRollback, "50.00", "w"), Reference{Tx: win})
	expectRejected(t, d, CodeReversalInsufficientFunds)
	expectBalance(t, f, "20.00", 3)
}

func TestPendingReferenceLifecycle(t *testing.T) {
	f := newFixture(t, "100.00")
	tx, d := f.apply(f.request(KindRefund, "40.00", "bet-late"), Reference{})
	if d.Outcome != OutcomeAwaitingReference || d.Entry != nil || tx.Status() != StatusPendingReference {
		t.Fatalf("decision = %+v, status %s", d, tx.Status())
	}
	if len(d.Events) != 1 {
		t.Fatalf("events = %d, want WagerTransactionPendingReference", len(d.Events))
	}
	if e, ok := d.Events[0].(event.WagerTransactionPendingReference); !ok || e.ReferenceExternalTransactionID != "bet-late" {
		t.Fatalf("event %#v", d.Events[0])
	}
	// Attempts counts lookups that found nothing, the first one included.
	if tx.Attempts() != 1 || !tx.ExpiresAt().Equal(f.now.Add(15*time.Minute)) || !tx.NextAttemptAt().Equal(f.now.Add(time.Second)) {
		t.Fatalf("schedule: attempts %d expires %v next %v", tx.Attempts(), tx.ExpiresAt(), tx.NextAttemptAt())
	}

	// Still missing: retry silently.
	d = f.reapply(tx, Reference{})
	if d.Outcome != OutcomeAwaitingReference || len(d.Events) != 0 || tx.Attempts() != 2 {
		t.Fatalf("retry: %+v attempts %d", d, tx.Attempts())
	}

	// The bet arrives (debit 40), the worker re-applies: refund processed.
	bet := f.processed(f.request(KindBet, "40.00", ""), Reference{})
	d = f.reapply(tx, Reference{Tx: bet})
	if d.Outcome != OutcomeProcessed || tx.Status() != StatusProcessed {
		t.Fatalf("resolution: %+v", d)
	}
	expectBalance(t, f, "100.00", 3)
}

func TestPendingReferenceExpires(t *testing.T) {
	f := newFixture(t, "100.00")
	tx, _ := f.apply(f.request(KindRefund, "40.00", "never"), Reference{})
	f.now = tx.ExpiresAt()
	d := f.reapply(tx, Reference{})
	expectRejected(t, d, CodeReferenceNotFound)
	if tx.Status() != StatusRejected {
		t.Fatalf("status %s", tx.Status())
	}
}

func TestPendingReferenceMaxAttempts(t *testing.T) {
	f := newFixture(t, "100.00")
	tx, _ := f.apply(f.request(KindRefund, "40.00", "never"), Reference{})
	for i := 0; i < 8; i++ {
		if d := f.reapply(tx, Reference{}); d.Outcome != OutcomeAwaitingReference {
			t.Fatalf("attempt %d: %s", i+1, d.Outcome)
		}
	}
	expectRejected(t, f.reapply(tx, Reference{}), CodeReferenceNotFound) // 10th attempt
}

// TestExpiryWhileReferenceIsPending: the reference exists but never finished,
// so the expiry reason is "not processed", not "not found".
func TestExpiryWhileReferenceIsPending(t *testing.T) {
	f := newFixture(t, "100.00")
	pending := newExternal(t, f.request(KindBet, "40.00", ""))
	tx, _ := f.apply(f.request(KindRefund, "40.00", "p"), Reference{Tx: pending})
	f.now = tx.ExpiresAt()
	expectRejected(t, f.reapply(tx, Reference{Tx: pending}), CodeReferenceNotProcessed)
}

func TestApplyRefusesTerminalTransactions(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := f.bet("10.00")
	_, err := tx.Apply(ApplyInput{Wallet: f.w, EntryID: uuid.New(), Retry: f.policy(), Now: f.now})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}
	expectBalance(t, f, "90.00", 2)
}

func TestApplyRefusesTheWrongWallet(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := newExternal(t, f.request(KindBet, "10.00", ""))
	other := newFixture(t, "100.00")
	_, err := tx.Apply(ApplyInput{Wallet: other.w, EntryID: uuid.New(), Retry: f.policy(), Now: f.now})
	if !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("err = %v, want ErrInvalidTransaction", err)
	}
}

func mustUSD(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "USD")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
