//go:build integration

package sqsclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

// The queues provisioned by docker compose carry exactly the resource
// policies of deploy/localstack/iam (whose least privilege is verified in
// internal/archtest; LocalStack Community does not enforce them).
func TestProvisionedQueuesCarryTheirPolicies(t *testing.T) {
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		t.Fatal("AWS_ENDPOINT_URL is not set; export the variables from .env.example")
	}
	client, err := NewClient(config.AWS{Region: os.Getenv("AWS_REGION"), EndpointURL: os.Getenv("AWS_ENDPOINT_URL")})
	if err != nil {
		t.Fatal(err)
	}
	for queue, file := range map[string]string{
		"wager-transactions.fifo":     "queue-wager-transactions.json",
		"wager-transactions-dlq.fifo": "queue-wager-transactions-dlq.json",
		"wallet-events.fifo":          "queue-wallet-events.json",
	} {
		url, err := client.GetQueueUrl(bg, &sqs.GetQueueUrlInput{QueueName: aws.String(queue)})
		if err != nil {
			t.Fatalf("%s: %v", queue, err)
		}
		out, err := client.GetQueueAttributes(bg, &sqs.GetQueueAttributesInput{
			QueueUrl: url.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNamePolicy}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "localstack", "iam", file))
		if err != nil {
			t.Fatal(err)
		}
		var deployed, want any
		if err := json.Unmarshal([]byte(out.Attributes["Policy"]), &deployed); err != nil {
			t.Fatalf("%s has no valid policy: %q", queue, out.Attributes["Policy"])
		}
		_ = json.Unmarshal(raw, &want)
		if !reflect.DeepEqual(deployed, want) {
			t.Errorf("%s policy differs from %s", queue, file)
		}
	}
}
