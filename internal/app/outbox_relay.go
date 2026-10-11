package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// PendingEvent is an outbox row claimed for publication.
type PendingEvent struct {
	ID          uuid.UUID
	AggregateID uuid.UUID
	Payload     []byte
	Attempts    int
	OccurredAt  time.Time
}

// OutboxStore claims and settles outbox rows. Claims are leases: a row whose
// owner died is claimable again once the lease expires.
type OutboxStore interface {
	// Claim leases up to limit due, unpublished rows to owner, skipping rows
	// other publishers hold, oldest first.
	Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]PendingEvent, error)
	// MarkPublished and MarkFailed only touch rows still leased to owner.
	MarkPublished(ctx context.Context, id uuid.UUID, owner string) error
	MarkFailed(ctx context.Context, id uuid.UUID, owner string, next time.Time, reason string) error
	// OldestUnpublished returns the occurred_at of the oldest pending event.
	OldestUnpublished(ctx context.Context) (time.Time, bool, error)
}

// EventSender publishes one event to the broker.
type EventSender interface {
	Send(ctx context.Context, e PendingEvent) error
}

type OutboxMetrics interface {
	OutboxPublished()
	OutboxPublishFailed()
	OutboxLag(time.Duration)
}

type NopOutboxMetrics struct{}

func (NopOutboxMetrics) OutboxPublished()        {}
func (NopOutboxMetrics) OutboxPublishFailed()    {}
func (NopOutboxMetrics) OutboxLag(time.Duration) {}

type OutboxRelayConfig struct {
	Owner        string
	BatchSize    int
	Lease        time.Duration
	PollInterval time.Duration
	BackoffBase  time.Duration
	BackoffCap   time.Duration
}

// OutboxRelay publishes committed events at least once. Every instance runs
// one; SKIP LOCKED and leases keep them from publishing the same row in the
// normal path. A crash between send and mark republishes the same eventId,
// which consumers deduplicate. Events are never dropped: failures are
// retried with backoff for as long as it takes.
type OutboxRelay struct {
	store   OutboxStore
	sender  EventSender
	clock   Clock
	metrics OutboxMetrics
	log     *slog.Logger
	cfg     OutboxRelayConfig
}

func NewOutboxRelay(store OutboxStore, sender EventSender, clock Clock, metrics OutboxMetrics, log *slog.Logger, cfg OutboxRelayConfig) *OutboxRelay {
	return &OutboxRelay{store: store, sender: sender, clock: clock, metrics: metrics, log: log.With("component", "outbox"), cfg: cfg}
}

// RunOnce publishes one batch and returns how many rows it claimed.
func (r *OutboxRelay) RunOnce(ctx context.Context) (int, error) {
	batch, err := r.store.Claim(ctx, r.cfg.Owner, r.cfg.BatchSize, r.cfg.Lease)
	if err != nil {
		return 0, err
	}
	for _, e := range batch {
		if err := r.sender.Send(ctx, e); err != nil {
			r.metrics.OutboxPublishFailed()
			next := r.clock.Now().Add(Backoff(e.Attempts+1, r.cfg.BackoffBase, r.cfg.BackoffCap))
			r.log.WarnContext(ctx, "outbox publish failed", "eventId", e.ID, "attempts", e.Attempts+1, "err", err)
			if err := r.store.MarkFailed(ctx, e.ID, r.cfg.Owner, next, err.Error()); err != nil {
				return len(batch), err
			}
			continue
		}
		if err := r.store.MarkPublished(ctx, e.ID, r.cfg.Owner); err != nil {
			// Already sent: the lease expires and the event is sent again
			// with the same eventId.
			return len(batch), err
		}
		r.metrics.OutboxPublished()
	}
	return len(batch), nil
}

// Run polls until ctx is cancelled. A full batch is followed immediately by
// the next one; otherwise it waits PollInterval.
func (r *OutboxRelay) Run(ctx context.Context) {
	for {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.WarnContext(ctx, "outbox relay iteration failed", "err", err)
		}
		r.reportLag(ctx)
		if n == r.cfg.BatchSize && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.PollInterval):
		}
	}
}

func (r *OutboxRelay) reportLag(ctx context.Context) {
	oldest, ok, err := r.store.OldestUnpublished(ctx)
	if err != nil {
		return
	}
	lag := time.Duration(0)
	if ok {
		lag = r.clock.Now().Sub(oldest)
	}
	r.metrics.OutboxLag(lag)
}
