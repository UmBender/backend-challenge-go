//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/adapters/sqsclient"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

// testQueue creates an isolated FIFO queue in LocalStack, deleted at the end.
func testQueue(t *testing.T) (*sqs.Client, string) {
	t.Helper()
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Fatal("AWS_ENDPOINT_URL is not set; export the variables from .env.example")
	}
	client, err := sqsclient.NewClient(config.AWS{Region: os.Getenv("AWS_REGION"), EndpointURL: os.Getenv("AWS_ENDPOINT_URL")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("outbox-test-" + uuid.NewString()[:8] + ".fifo"),
		Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"},
	})
	if err != nil {
		t.Fatalf("create queue (is LocalStack up?): %v", err)
	}
	t.Cleanup(func() { _, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl}) })
	return client, *out.QueueUrl
}

// drain receives and deletes every message currently in the queue.
func drain(t *testing.T, client *sqs.Client, url string) []types.Message {
	t.Helper()
	var all []types.Message
	for empty := 0; empty < 2; {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameMessageGroupId, types.MessageSystemAttributeNameMessageDeduplicationId,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Messages) == 0 {
			empty++
			continue
		}
		for _, m := range out.Messages {
			all = append(all, m)
			_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle})
		}
	}
	return all
}

// countingSender records how many times each event was sent.
type countingSender struct {
	next app.EventSender
	mu   sync.Mutex
	sent map[uuid.UUID]int
}

func (c *countingSender) Send(ctx context.Context, e app.PendingEvent) error {
	if err := c.next.Send(ctx, e); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent[e.ID]++
	return nil
}

func relayFor(e *env, sender app.EventSender, owner string, batch int) *app.OutboxRelay {
	return app.NewOutboxRelay(postgres.NewOutboxStore(e.pool), sender, app.SystemClock{}, app.NopOutboxMetrics{},
		slog.New(slog.DiscardHandler), app.OutboxRelayConfig{
			Owner: owner, BatchSize: batch, Lease: 30 * time.Second, BackoffBase: time.Second, BackoffCap: time.Minute,
		})
}

func sender(client *sqs.Client, url string) app.EventSender {
	return sqsclient.NewEventSender(client, &sqsclient.Queues{EventsURL: url})
}

func TestOutboxRelayPublishesCommittedEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	client, url := testQueue(t)
	w := e.wallet("100.00") // opening: two events committed with the wallet

	n, err := relayFor(e, sender(client, url), "relay-1", 10).RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	msgs := drain(t, client, url)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	for _, m := range msgs {
		group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		dedup := m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)]
		var body struct {
			EventID     string `json:"eventId"`
			AggregateID string `json:"aggregateId"`
		}
		if err := json.Unmarshal([]byte(*m.Body), &body); err != nil {
			t.Fatal(err)
		}
		if group != w.ID().String() || body.EventID != dedup || body.AggregateID != group {
			t.Fatalf("group %q, dedup %q, body %s", group, dedup, *m.Body)
		}
	}
	if left := e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); left != 0 {
		t.Fatalf("%d events left unpublished", left)
	}
}

func TestOutboxRelayConcurrentPublishersSendEachEventOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	client, url := testQueue(t)
	for range 10 {
		e.wallet("10.00")
	}
	counter := &countingSender{next: sender(client, url), sent: map[uuid.UUID]int{}}
	var wg sync.WaitGroup
	for _, owner := range []string{"relay-a", "relay-b"} {
		r := relayFor(e, counter, owner, 3)
		wg.Go(func() {
			for {
				n, err := r.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if len(counter.sent) != 20 {
		t.Fatalf("distinct events sent = %d, want 20", len(counter.sent))
	}
	for id, n := range counter.sent {
		if n != 1 {
			t.Fatalf("event %s sent %d times", id, n)
		}
	}
	if left := e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); left != 0 {
		t.Fatalf("%d events left unpublished", left)
	}
}

func TestOutboxRelayKeepsEventsWhileTheBrokerFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	client, url := testQueue(t)
	e.wallet("100.00")

	broken := sender(client, url+"-missing")
	if _, err := relayFor(e, broken, "relay-1", 10).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM outbox_events
		WHERE published_at IS NULL AND attempts = 1 AND last_error IS NOT NULL AND next_attempt_at > now() AND locked_by IS NULL`); n != 2 {
		t.Fatalf("events rescheduled after failure = %d, want 2", n)
	}
	// When the broker is back and the backoff elapsed, they are published.
	if _, err := e.check.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n, err := relayFor(e, sender(client, url), "relay-1", 10).RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RunOnce after recovery = %d, %v", n, err)
	}
	if got := len(drain(t, client, url)); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}
}

func TestOutboxRelayReclaimsAnExpiredLease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	client, url := testQueue(t)
	e.wallet("100.00")

	// A publisher claims the events and dies before sending them.
	store := postgres.NewOutboxStore(e.pool)
	if claimed, err := store.Claim(ctx, "dead-relay", 10, 300*time.Millisecond); err != nil || len(claimed) != 2 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	survivor := relayFor(e, sender(client, url), "relay-2", 10)
	if n, _ := survivor.RunOnce(ctx); n != 0 {
		t.Fatalf("leased events were taken before the lease expired: %d", n)
	}
	time.Sleep(400 * time.Millisecond)
	if n, err := survivor.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RunOnce after lease expiry = %d, %v", n, err)
	}
	if got := len(drain(t, client, url)); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}
}

// §11: a publisher sends the events and dies before marking them published.
// After the lease expires another publisher sends them again with the same
// eventId; SQS FIFO deduplication drops the copies, and consumers deduplicate
// on eventId beyond its window.
func TestOutboxRelayRepublishesWithTheSameEventID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	client, url := testQueue(t)
	e.wallet("100.00")

	store := postgres.NewOutboxStore(e.pool)
	claimed, err := store.Claim(ctx, "dead-relay", 10, 300*time.Millisecond)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	for _, ev := range claimed {
		if err := sender(client, url).Send(ctx, ev); err != nil { // published, never marked
			t.Fatal(err)
		}
	}

	time.Sleep(400 * time.Millisecond)
	counter := &countingSender{next: sender(client, url), sent: map[uuid.UUID]int{}}
	if n, err := relayFor(e, counter, "relay-2", 10).RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RunOnce after lease expiry = %d, %v", n, err)
	}
	for _, ev := range claimed {
		if counter.sent[ev.ID] != 1 {
			t.Fatalf("event %s was not republished with its original eventId: %v", ev.ID, counter.sent)
		}
	}
	if left := e.count(`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`); left != 0 {
		t.Fatalf("%d events left unpublished", left)
	}
	msgs := drain(t, client, url)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (the copies are deduplicated by eventId)", len(msgs))
	}
	for _, m := range msgs {
		dedup := m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)]
		if _, err := uuid.Parse(dedup); err != nil {
			t.Fatalf("deduplication id %q is not the eventId", dedup)
		}
	}
}
