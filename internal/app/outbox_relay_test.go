package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeOutbox struct {
	mu        sync.Mutex
	pending   []PendingEvent
	published []uuid.UUID
	failed    map[uuid.UUID]time.Time
}

func (f *fakeOutbox) Claim(_ context.Context, _ string, limit int, _ time.Duration) ([]PendingEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := min(limit, len(f.pending))
	out := f.pending[:n]
	f.pending = f.pending[n:]
	return out, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ids []uuid.UUID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, ids...)
	return nil
}

func (f *fakeOutbox) MarkFailed(_ context.Context, id uuid.UUID, _ string, next time.Time, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = next
	return nil
}

func (f *fakeOutbox) OldestUnpublished(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

type fakeSender struct{ fail map[uuid.UUID]bool }

func (s fakeSender) Send(_ context.Context, events []PendingEvent) []error {
	errs := make([]error, len(events))
	for i, e := range events {
		if s.fail[e.ID] {
			errs[i] = errors.New("broker unavailable")
		}
	}
	return errs
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func TestOutboxRelayPublishesAndBacksOffFailures(t *testing.T) {
	ok1, bad, ok2 := uuid.New(), uuid.New(), uuid.New()
	store := &fakeOutbox{failed: map[uuid.UUID]time.Time{}, pending: []PendingEvent{{ID: ok1}, {ID: bad, Attempts: 2}, {ID: ok2}}}
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	relay := NewOutboxRelay(store, fakeSender{fail: map[uuid.UUID]bool{bad: true}}, fixedClock{now}, NopOutboxMetrics{},
		slog.New(slog.DiscardHandler), OutboxRelayConfig{Owner: "test", BatchSize: 10, BackoffBase: time.Second, BackoffCap: time.Minute})

	n, err := relay.RunOnce(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if len(store.published) != 2 || store.published[0] != ok1 || store.published[1] != ok2 {
		t.Fatalf("published = %v; a failure must not stop the batch", store.published)
	}
	next, ok := store.failed[bad]
	if !ok || !next.After(now) {
		t.Fatalf("failed event not rescheduled in the future: %v", store.failed)
	}
}
