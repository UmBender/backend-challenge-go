package sqsclient

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

// EventSender publishes outbox events to the events FIFO queue. The wallet is
// the MessageGroupId (per-wallet ordering) and the eventId is the
// MessageDeduplicationId, so a republication within the SQS deduplication
// window is dropped by the broker; consumers deduplicate on eventId beyond it.
type EventSender struct {
	client   *sqs.Client
	queueURL func() string
}

func NewEventSender(client *sqs.Client, queues *Queues) *EventSender {
	// Queue URLs are resolved on start, after construction.
	return &EventSender{client: client, queueURL: func() string { return queues.EventsURL }}
}

func (s *EventSender) Send(ctx context.Context, e app.PendingEvent) error {
	_, err := s.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(s.queueURL()),
		MessageBody:            aws.String(string(e.Payload)),
		MessageGroupId:         aws.String(e.AggregateID.String()),
		MessageDeduplicationId: aws.String(e.ID.String()),
	})
	return err
}
