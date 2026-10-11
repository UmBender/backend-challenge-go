package sqsclient

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

// sqsBatchLimit is the SQS maximum of entries per SendMessageBatch.
const sqsBatchLimit = 10

// EventSender publishes outbox events to the events FIFO queue. The wallet is
// the MessageGroupId (per-wallet ordering) and the eventId is the
// MessageDeduplicationId, so a republication within the SQS deduplication
// window is dropped by the broker; consumers deduplicate on eventId beyond it.
// Within one batch SQS keeps the order of entries of the same group.
type EventSender struct {
	client   *sqs.Client
	queueURL func() string
}

func NewEventSender(client *sqs.Client, queues *Queues) *EventSender {
	// Queue URLs are resolved on start, after construction.
	return &EventSender{client: client, queueURL: func() string { return queues.EventsURL }}
}

func (s *EventSender) Send(ctx context.Context, events []app.PendingEvent) []error {
	errs := make([]error, len(events))
	for start := 0; start < len(events); start += sqsBatchLimit {
		chunk := events[start:min(start+sqsBatchLimit, len(events))]
		entries := make([]types.SendMessageBatchRequestEntry, len(chunk))
		for i, e := range chunk {
			entries[i] = types.SendMessageBatchRequestEntry{
				Id:                     aws.String(strconv.Itoa(i)),
				MessageBody:            aws.String(string(e.Payload)),
				MessageGroupId:         aws.String(e.AggregateID.String()),
				MessageDeduplicationId: aws.String(e.ID.String()),
			}
		}
		out, err := s.client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(s.queueURL()), Entries: entries})
		if err != nil {
			for i := range chunk {
				errs[start+i] = err
			}
			continue
		}
		for _, f := range out.Failed {
			i, _ := strconv.Atoi(aws.ToString(f.Id))
			errs[start+i] = fmt.Errorf("%s: %s", aws.ToString(f.Code), aws.ToString(f.Message))
		}
	}
	return errs
}
