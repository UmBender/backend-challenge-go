package app

import (
	"time"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
)

// envelopes adds the metadata only the application knows.
func envelopes(ids IDs, correlationID, causationID string, at time.Time, payloads []event.Payload) ([]event.Envelope, error) {
	out := make([]event.Envelope, 0, len(payloads))
	for _, p := range payloads {
		e, err := event.New(event.Meta{
			EventID: ids.New(), CorrelationID: correlationID, CausationID: causationID, OccurredAt: at,
		}, p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
