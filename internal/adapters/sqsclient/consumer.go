package sqsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

const (
	consumerName = "wager-transactions"
	messageType  = "WagerTransactionRequested"
)

// Submitter is app.SubmitWager, the use case shared with HTTP.
type Submitter interface {
	Execute(ctx context.Context, cmd app.SubmitCommand) (app.SubmitResult, error)
}

type ConsumerConfig struct {
	// Groups processed in parallel; messages of one group run in order.
	Concurrency int
	WaitTime    int32
	// Bounds one message; must stay well under the queue visibility timeout.
	ProcessTimeout time.Duration
	// Visibility delay after a transient failure, growing with the receive count.
	RetryBase time.Duration
	RetryCap  time.Duration
}

// Consumer reads wager operations from the input FIFO queue. A message is
// deleted only after its handling committed (inbox included). Permanent
// failures are forwarded to the DLQ; transient ones become visible again
// after a backoff, and the queue's redrive policy moves them to the DLQ once
// maxReceiveCount is reached.
type Consumer struct {
	client   *sqs.Client
	queues   *Queues
	submit   Submitter
	log      *slog.Logger
	cfg      ConsumerConfig
	inputURL func() string
}

func NewConsumer(client *sqs.Client, queues *Queues, submit Submitter, log *slog.Logger, cfg ConsumerConfig) *Consumer {
	return &Consumer{client: client, queues: queues, submit: submit, log: log.With("component", "sqs-consumer"), cfg: cfg,
		inputURL: func() string { return queues.WagerURL }}
}

// Run polls until ctx is cancelled, then waits for the messages in progress.
// In-flight work is not cancelled by ctx; messages not yet started when ctx
// ends are released for redelivery.
func (c *Consumer) Run(ctx context.Context) {
	sem := make(chan struct{}, c.cfg.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		out, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(c.inputURL()), MaxNumberOfMessages: 10, WaitTimeSeconds: c.cfg.WaitTime,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameMessageGroupId, types.MessageSystemAttributeNameApproximateReceiveCount,
			},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.WarnContext(ctx, "receive failed", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		for _, group := range byGroup(out.Messages) {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				c.release(group)
				continue
			}
			wg.Go(func() {
				defer func() { <-sem }()
				c.processGroup(ctx, group)
			})
		}
	}
}

func (c *Consumer) processGroup(ctx context.Context, msgs []types.Message) {
	for i, m := range msgs {
		if ctx.Err() != nil {
			c.release(msgs[i:])
			return
		}
		if !c.handle(context.WithoutCancel(ctx), m) {
			// Keep the group's order: the rest waits for the failed message.
			c.retryLater(msgs[i+1:], receiveCount(m))
			return
		}
	}
}

// handle reports whether the message was settled (deleted or sent to the DLQ).
func (c *Consumer) handle(base context.Context, m types.Message) bool {
	ctx, cancel := context.WithTimeout(base, c.cfg.ProcessTimeout)
	defer cancel()
	cmd, err := parseMessage([]byte(aws.ToString(m.Body)))
	if err == nil {
		var res app.SubmitResult
		res, err = c.submit.Execute(ctx, cmd)
		if err == nil {
			c.log.InfoContext(ctx, "message processed", "messageId", cmd.Inbox.MessageID,
				"transactionId", res.Transaction.ID(), "status", res.Transaction.Status(), "replay", res.Replay)
			return c.delete(ctx, m)
		}
	}
	if permanent(err) {
		c.log.WarnContext(ctx, "message rejected permanently, forwarding to the DLQ", "sqsMessageId", aws.ToString(m.MessageId), "err", err)
		return c.toDLQ(ctx, m, err)
	}
	c.log.WarnContext(ctx, "message failed transiently", "sqsMessageId", aws.ToString(m.MessageId), "receiveCount", receiveCount(m), "err", err)
	c.retryLater([]types.Message{m}, receiveCount(m))
	return false
}

func permanent(err error) bool {
	var inv *wager.InvalidRequestError
	return errors.As(err, &inv) || errors.Is(err, errMalformed) ||
		errors.Is(err, money.ErrInvalidFormat) || errors.Is(err, money.ErrScale) || errors.Is(err, money.ErrNegative) ||
		errors.Is(err, money.ErrOverflow) || errors.Is(err, money.ErrInvalidCurrency) ||
		errors.Is(err, app.ErrIdempotencyKeyReused) || errors.Is(err, app.ErrExternalTransactionConflict) ||
		errors.Is(err, app.ErrWalletNotFound) || errors.Is(err, app.ErrMessageReused) || errors.Is(err, app.ErrInvariantViolation)
}

func (c *Consumer) delete(ctx context.Context, m types.Message) bool {
	_, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.inputURL()), ReceiptHandle: m.ReceiptHandle})
	if err != nil {
		// Committed but not deleted: the redelivery is a replay through the inbox.
		c.log.WarnContext(ctx, "delete failed", "sqsMessageId", aws.ToString(m.MessageId), "err", err)
	}
	return true
}

func (c *Consumer) toDLQ(ctx context.Context, m types.Message, reason error) bool {
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "invalid"
	}
	_, err := c.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(c.queues.DLQURL), MessageBody: m.Body,
		MessageGroupId: aws.String(group), MessageDeduplicationId: m.MessageId,
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failureReason": {DataType: aws.String("String"), StringValue: aws.String(truncate(reason.Error(), 256))},
		},
	})
	if err != nil {
		c.log.WarnContext(ctx, "DLQ forward failed", "sqsMessageId", aws.ToString(m.MessageId), "err", err)
		c.retryLater([]types.Message{m}, receiveCount(m))
		return false
	}
	return c.delete(ctx, m)
}

func (c *Consumer) retryLater(msgs []types.Message, attempt int) {
	delay := app.Backoff(attempt, c.cfg.RetryBase, c.cfg.RetryCap)
	c.setVisibility(msgs, int32(max(1, delay/time.Second)))
}

// release makes messages visible again immediately (shutdown).
func (c *Consumer) release(msgs []types.Message) { c.setVisibility(msgs, 0) }

func (c *Consumer) setVisibility(msgs []types.Message, seconds int32) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, m := range msgs {
		_, _ = c.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.inputURL()), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: seconds,
		})
	}
}

func byGroup(msgs []types.Message) [][]types.Message {
	var order []string
	groups := map[string][]types.Message{}
	for _, m := range msgs {
		g := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], m)
	}
	out := make([][]types.Message, 0, len(order))
	for _, g := range order {
		out = append(out, groups[g])
	}
	return out
}

func receiveCount(m types.Message) int {
	n, err := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

var errMalformed = errors.New("malformed message")

type envelope struct {
	MessageID  string          `json:"messageId"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

type requestData struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	IdempotencyKey                 string      `json:"idempotencyKey"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
}

// parseMessage builds the same command the HTTP path builds; the business
// validation is the use case's. Unknown fields are rejected.
func parseMessage(body []byte) (app.SubmitCommand, error) {
	var env envelope
	if err := strictDecode(body, &env); err != nil {
		return app.SubmitCommand{}, err
	}
	if env.MessageID == "" || len(env.MessageID) > 255 || env.Type != messageType || len(env.Data) == 0 {
		return app.SubmitCommand{}, fmt.Errorf("%w: messageId, type %q and data are required", errMalformed, messageType)
	}
	var d requestData
	if err := strictDecode(env.Data, &d); err != nil {
		return app.SubmitCommand{}, err
	}
	player, err := canonicalUUID(d.PlayerID)
	if err != nil {
		return app.SubmitCommand{}, err
	}
	walletID, err := canonicalUUID(d.WalletID)
	if err != nil {
		return app.SubmitCommand{}, err
	}
	return app.SubmitCommand{
		Request: wager.Request{
			ProviderID: d.ProviderID, ExternalTransactionID: d.ExternalTransactionID, PlayerID: player, WalletID: walletID,
			RoundID: d.RoundID, GameID: d.GameID, Kind: wager.Kind(d.Kind), Money: d.Money,
			ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
		},
		IdempotencyKey: d.IdempotencyKey,
		CorrelationID:  d.IdempotencyKey,
		CausationID:    env.MessageID,
		Inbox:          &app.InboxMessage{Consumer: consumerName, MessageID: env.MessageID},
	}, nil
}

func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %w", errMalformed, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", errMalformed)
	}
	return nil
}

func canonicalUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("%w: %q is not a canonical UUID", errMalformed, s)
	}
	return id, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
