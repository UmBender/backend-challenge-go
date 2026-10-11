#!/bin/bash
# LocalStack runs this once it is ready (init hook "ready" stage). Creates the
# input FIFO queue with its DLQ redrive, the DLQ itself and the outgoing
# events queue. Idempotent: CreateQueue with identical attributes is a no-op.
set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
SQS="awslocal --region ${REGION} sqs"

WAGER_QUEUE="${SQS_WAGER_QUEUE_NAME:-wager-transactions.fifo}"
DLQ="${SQS_DLQ_NAME:-wager-transactions-dlq.fifo}"
EVENTS_QUEUE="${SQS_EVENTS_QUEUE_NAME:-wallet-events.fifo}"
MAX_RECEIVE_COUNT="${SQS_MAX_RECEIVE_COUNT:-5}"
VISIBILITY_TIMEOUT="${SQS_VISIBILITY_TIMEOUT:-60}"

# 14 days: the maximum, so failed messages can be inspected and redriven.
$SQS create-queue --queue-name "$DLQ" \
    --attributes FifoQueue=true,MessageRetentionPeriod=1209600 >/dev/null
DLQ_URL=$($SQS get-queue-url --queue-name "$DLQ" --query QueueUrl --output text)
DLQ_ARN=$($SQS get-queue-attributes --queue-url "$DLQ_URL" \
    --attribute-names QueueArn --query Attributes.QueueArn --output text)

# A file avoids escaping a JSON redrive policy inside a JSON attribute map.
ATTRS=$(mktemp)
cat >"$ATTRS" <<JSON
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "VisibilityTimeout": "${VISIBILITY_TIMEOUT}",
  "ReceiveMessageWaitTimeSeconds": "20",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_ARN}\",\"maxReceiveCount\":\"${MAX_RECEIVE_COUNT}\"}"
}
JSON
$SQS create-queue --queue-name "$WAGER_QUEUE" --attributes "file://${ATTRS}" >/dev/null
rm -f "$ATTRS"

$SQS create-queue --queue-name "$EVENTS_QUEUE" \
    --attributes FifoQueue=true,ContentBasedDeduplication=false >/dev/null

# Resource policies: only the expected principals may use each queue.
set_policy() {
	local name="$1" file="$2" url
	url=$($SQS get-queue-url --queue-name "$name" --query QueueUrl --output text)
	python3 -c 'import json,sys; print(json.dumps({"Policy": json.dumps(json.load(open(sys.argv[1])))}))' "$file" >"$ATTRS"
	$SQS set-queue-attributes --queue-url "$url" --attributes "file://${ATTRS}"
}
ATTRS=$(mktemp)
set_policy "$WAGER_QUEUE" /etc/localstack/iam/queue-wager-transactions.json
set_policy "$DLQ" /etc/localstack/iam/queue-wager-transactions-dlq.json
set_policy "$EVENTS_QUEUE" /etc/localstack/iam/queue-wallet-events.json
rm -f "$ATTRS"

echo "SQS queues ready: ${WAGER_QUEUE} (DLQ ${DLQ}, maxReceiveCount ${MAX_RECEIVE_COUNT}), ${EVENTS_QUEUE}"
