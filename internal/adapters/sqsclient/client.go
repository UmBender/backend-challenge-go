// Package sqsclient builds the AWS SQS client and resolves the queue URLs the
// service needs. The consumer (T9) and the outbox publisher (T7) depend on it.
package sqsclient

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

// Module provides the SQS client, the resolved Queues and the SQS readiness check.
var Module = fx.Module("sqs",
	fx.Provide(
		NewClient,
		NewQueues,
		fx.Annotate(NewEventSender, fx.As(new(app.EventSender))),
		health.AsCheck(NewCheck),
	),
)

// NewClient loads AWS configuration (credentials from the standard
// AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY chain) and points the client at
// LocalStack/MiniStack when AWS_ENDPOINT_URL is set. Loading the config does
// no network I/O when credentials come from the environment, so a background
// context is enough here; every SQS call later gets its own deadline.
func NewClient(cfg config.AWS) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.EndpointURL)
		}
	}), nil
}

// Queues holds the resolved queue URLs. They are filled in OnStart, so use
// them only from components started after this one.
type Queues struct {
	WagerURL  string
	DLQURL    string
	EventsURL string
}

// NewQueues resolves every queue on start: a missing queue (provisioning not
// done, wrong name, wrong account) fails startup instead of the first message.
func NewQueues(lc fx.Lifecycle, client *sqs.Client, names config.SQS, log *slog.Logger) *Queues {
	q := &Queues{}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		for name, dst := range map[string]*string{
			names.WagerQueueName:  &q.WagerURL,
			names.DLQName:         &q.DLQURL,
			names.EventsQueueName: &q.EventsURL,
		} {
			out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
			if err != nil {
				return fmt.Errorf("resolve SQS queue %q: %w", name, err)
			}
			*dst = aws.ToString(out.QueueUrl)
		}
		log.Info("sqs queues resolved", "component", "sqs",
			"wagerQueue", names.WagerQueueName, "dlq", names.DLQName, "eventsQueue", names.EventsQueueName)
		return nil
	}})
	return q
}

// Check reports SQS readiness by reading one attribute of the input queue.
type Check struct {
	client *sqs.Client
	queues *Queues
}

// NewCheck is the Fx constructor for the readiness check.
func NewCheck(client *sqs.Client, queues *Queues) *Check {
	return &Check{client: client, queues: queues}
}

// Name implements health.Check.
func (c *Check) Name() string { return "sqs" }

// Check implements health.Check.
func (c *Check) Check(ctx context.Context) error {
	_, err := c.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(c.queues.WagerURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	return err
}
