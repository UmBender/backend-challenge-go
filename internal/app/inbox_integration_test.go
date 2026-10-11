//go:build integration

package app_test

import (
	"errors"
	"testing"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

func (e *env) consume(r wager.Request, key, messageID string) (app.SubmitResult, error) {
	return e.submitter().Execute(ctx, app.SubmitCommand{
		Request: r, IdempotencyKey: key, CausationID: messageID,
		Inbox: &app.InboxMessage{Consumer: "wager-transactions", MessageID: messageID},
	})
}

func TestInboxRedeliveryHasOneEffect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	bet := e.request(w, wager.KindBet, "25.00", "bet-1", "")

	first, err := e.consume(bet, "provider-a:bet-1", "msg-1")
	if err != nil || first.Replay {
		t.Fatalf("first delivery: %+v, %v", first, err)
	}
	again, err := e.consume(bet, "provider-a:bet-1", "msg-1")
	if err != nil || !again.Replay || again.Transaction.ID() != first.Transaction.ID() {
		t.Fatalf("redelivery: %+v, %v", again, err)
	}
	if minor, _ := e.balance(w); minor != 7500 {
		t.Fatalf("balance = %d, want 7500", minor)
	}
	if n := e.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL AND transaction_id IS NOT NULL`); n != 1 {
		t.Fatalf("completed inbox rows = %d", n)
	}
	e.assertReconciled(w)
}

func TestInboxRejectsAReusedMessageID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	if _, err := e.consume(e.request(w, wager.KindBet, "25.00", "bet-1", ""), "k1", "msg-1"); err != nil {
		t.Fatal(err)
	}
	_, err := e.consume(e.request(w, wager.KindBet, "30.00", "bet-2", ""), "k2", "msg-1")
	if !errors.Is(err, app.ErrMessageReused) {
		t.Fatalf("err = %v, want ErrMessageReused", err)
	}
	if minor, _ := e.balance(w); minor != 7500 {
		t.Fatalf("balance = %d, want 7500", minor)
	}
}

// The same operation sent over HTTP and then over SQS has one effect: the
// SQS path is a replay of the HTTP result.
func TestHTTPThenSQSIsOneOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	bet := e.request(w, wager.KindBet, "25.00", "bet-1", "")
	viaHTTP := e.mustSubmit(bet, "provider-a:bet-1")

	viaSQS, err := e.consume(bet, "provider-a:bet-1", "msg-1")
	if err != nil || !viaSQS.Replay || viaSQS.Transaction.ID() != viaHTTP.Transaction.ID() {
		t.Fatalf("SQS after HTTP: %+v, %v", viaSQS, err)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != 1 {
		t.Fatalf("debits = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM inbox_messages WHERE transaction_id = $1`, viaHTTP.Transaction.ID()); n != 1 {
		t.Fatal("inbox row not completed with the HTTP transaction")
	}
}

func TestInboxCompletesAPendingReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.wallet("100.00")
	res, err := e.consume(e.request(w, wager.KindRefund, "25.00", "refund-1", "bet-1"), "kr", "msg-1")
	if err != nil || res.Transaction.Status() != wager.StatusPendingReference {
		t.Fatalf("pending via SQS: %+v, %v", res, err)
	}
	if n := e.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`); n != 1 {
		t.Fatal("the message must be completed once the pending state is persisted")
	}
}
