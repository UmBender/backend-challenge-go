//go:build integration

package sqsclient

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres/pgtest"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

var bg = context.Background()

type harness struct {
	t      *testing.T
	client *sqs.Client
	queues *Queues
	check  *pgxpool.Pool
	tx     *postgres.TxManager
	wallet uuid.UUID
	player uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Fatal("AWS_ENDPOINT_URL is not set; export the variables from .env.example")
	}
	client, err := NewClient(config.AWS{Region: os.Getenv("AWS_REGION"), EndpointURL: os.Getenv("AWS_ENDPOINT_URL")})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, client: client, queues: &Queues{}}
	suffix := uuid.NewString()[:8]
	h.queues.DLQURL = h.createQueue("consumer-dlq-"+suffix+".fifo", nil)
	arn, err := client.GetQueueAttributes(bg, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(h.queues.DLQURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	h.queues.WagerURL = h.createQueue("consumer-in-"+suffix+".fifo", map[string]string{
		"VisibilityTimeout": "5",
		"RedrivePolicy":     fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"2"}`, arn.Attributes["QueueArn"]),
	})

	db := pgtest.New(t)
	h.check = db.Pool(t, pgtest.Owner)
	h.tx = postgres.NewTxManager(db.Pool(t, pgtest.App))
	balance, _ := money.Parse("100.00", "BRL")
	h.player = uuid.New()
	w, err := app.NewOpenWallet(h.tx, app.SystemClock{}, app.UUIDv7{}).Execute(bg, app.OpenWalletCommand{PlayerID: h.player, InitialBalance: balance})
	if err != nil {
		t.Fatal(err)
	}
	h.wallet = w.ID()
	return h
}

func (h *harness) createQueue(name string, attrs map[string]string) string {
	h.t.Helper()
	all := map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"}
	for k, v := range attrs {
		all[k] = v
	}
	out, err := h.client.CreateQueue(bg, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: all})
	if err != nil {
		h.t.Fatalf("create queue (is LocalStack up?): %v", err)
	}
	h.t.Cleanup(func() { _, _ = h.client.DeleteQueue(bg, &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl}) })
	return *out.QueueUrl
}

// start runs a consumer until the test ends.
func (h *harness) start(submit Submitter) {
	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	c := NewConsumer(h.client, h.queues, submit, slog.New(slog.DiscardHandler), ConsumerConfig{
		Concurrency: 4, WaitTime: 1, ProcessTimeout: 10 * time.Second, RetryBase: time.Second, RetryCap: time.Second,
	})
	wg.Go(func() { c.Run(ctx) })
	h.t.Cleanup(func() { cancel(); wg.Wait() })
}

func (h *harness) submitter() Submitter {
	return app.NewSubmitWager(h.tx, app.SystemClock{}, app.UUIDv7{},
		app.PendingPolicy{TTL: time.Minute, MaxAttempts: 5, BackoffBase: time.Second, BackoffCap: time.Minute})
}

func (h *harness) message(messageID, ext, amount string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",`+
		`"data":{"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":"provider-a:%s",`+
		`"playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":%q,"currency":"BRL"}}}`,
		messageID, ext, ext, h.player, h.wallet, amount)
}

func (h *harness) send(body string) {
	h.t.Helper()
	_, err := h.client.SendMessage(bg, &sqs.SendMessageInput{
		QueueUrl: aws.String(h.queues.WagerURL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(h.wallet.String()), MessageDeduplicationId: aws.String(uuid.NewString()),
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) pending(url string) int {
	out, err := h.client.GetQueueAttributes(bg, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		}})
	if err != nil {
		h.t.Fatal(err)
	}
	var a, b int
	fmt.Sscan(out.Attributes["ApproximateNumberOfMessages"], &a)
	fmt.Sscan(out.Attributes["ApproximateNumberOfMessagesNotVisible"], &b)
	return a + b
}

func (h *harness) count(sql string) int {
	h.t.Helper()
	var n int
	if err := h.check.QueryRow(bg, sql).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) eventually(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestConsumerProcessesAndDeletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start(h.submitter())
	h.send(h.message("msg-1", "bet-1", "25.00"))
	h.eventually("message processed and deleted", func() bool {
		return h.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == 1 && h.pending(h.queues.WagerURL) == 0
	})
	if n := h.count(`SELECT count(*) FROM wallets WHERE balance_minor = 7500`); n != 1 {
		t.Fatal("balance not debited")
	}
}

func TestConsumerRedeliveryHasOneEffect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	body := h.message("msg-1", "bet-1", "25.00")
	h.send(body)
	h.send(body) // a second delivery of the same message (distinct SQS dedup id)
	h.start(h.submitter())
	h.eventually("both deliveries settled", func() bool { return h.pending(h.queues.WagerURL) == 0 })
	if n := h.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != 1 {
		t.Fatalf("debits = %d, want 1", n)
	}
	if n := h.pending(h.queues.DLQURL); n != 0 {
		t.Fatalf("a duplicate went to the DLQ: %d", n)
	}
}

func TestConsumerSettlesBusinessRejections(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start(h.submitter())
	h.send(h.message("msg-1", "bet-1", "500.00"))
	h.eventually("rejection persisted and message deleted", func() bool {
		return h.count(`SELECT count(*) FROM wager_transactions WHERE failure_code = 'INSUFFICIENT_FUNDS'`) == 1 &&
			h.pending(h.queues.WagerURL) == 0
	})
	if n := h.pending(h.queues.DLQURL); n != 0 {
		t.Fatalf("a business rejection went to the DLQ: %d", n)
	}
}

func TestConsumerSendsMalformedMessagesToTheDLQ(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start(h.submitter())
	h.send(`{"messageId":"msg-1","type":"WagerTransactionRequested"`)
	h.send(strings.Replace(h.message("msg-2", "bet-2", "25.00"), `"kind":"BET"`, `"kind":"OPENING"`, 1))
	h.eventually("malformed messages in the DLQ", func() bool {
		return h.pending(h.queues.DLQURL) == 2 && h.pending(h.queues.WagerURL) == 0
	})
	if n := h.count(`SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`); n != 0 {
		t.Fatalf("a malformed message created %d transactions", n)
	}
}

type unavailable struct{}

func (unavailable) Execute(context.Context, app.SubmitCommand) (app.SubmitResult, error) {
	return app.SubmitResult{}, app.ErrUnavailable
}

// A dependency that stays down: the message is retried, then the redrive
// policy moves it to the DLQ instead of blocking its group forever.
func TestConsumerRetriesTransientFailuresUntilTheDLQ(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.start(unavailable{})
	h.send(h.message("msg-1", "bet-1", "25.00"))
	h.eventually("message redriven to the DLQ", func() bool {
		return h.pending(h.queues.DLQURL) == 1 && h.pending(h.queues.WagerURL) == 0
	})
}

// §13.5: a consumer commits the operation and dies before deleting the
// message. The message becomes visible again, is redelivered to another
// consumer and settled as a replay: one debit, nothing in the DLQ.
func TestConsumerCrashAfterCommitBeforeDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.send(h.message("msg-1", "bet-1", "25.00"))

	// The consumer that crashes: receive, commit, never delete.
	out, err := h.client.ReceiveMessage(bg, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(h.queues.WagerURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 2, VisibilityTimeout: 1,
	})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("receive: %v, %d messages", err, len(out.Messages))
	}
	cmd, err := parseMessage([]byte(*out.Messages[0].Body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.submitter().Execute(bg, cmd); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != 1 {
		t.Fatalf("debits after the first delivery = %d", n)
	}

	// After the visibility timeout another consumer gets the same message.
	h.start(h.submitter())
	h.eventually("redelivered message settled", func() bool { return h.pending(h.queues.WagerURL) == 0 })
	if n := h.count(`SELECT count(*) FROM wallet_ledger_entries WHERE direction = 'DEBIT'`); n != 1 {
		t.Fatalf("debits after redelivery = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`); n != 1 {
		t.Fatalf("completed inbox rows = %d, want 1", n)
	}
	if n := h.pending(h.queues.DLQURL); n != 0 {
		t.Fatalf("redelivery went to the DLQ: %d", n)
	}
}
