#!/bin/bash
# Least-privilege identities for the broker (§2), one per component. Runs
# before the queues are created. LocalStack Community stores these policies
# but does not enforce them; see ARCHITECTURE.md.
set -euo pipefail

IAM="awslocal iam"
POLICIES=/etc/localstack/iam

for user in wallet-service wager-producer events-consumer; do
	$IAM create-user --user-name "$user" >/dev/null
	$IAM put-user-policy --user-name "$user" --policy-name "$user" \
		--policy-document "file://${POLICIES}/${user}.json"
done

echo "IAM users ready: wallet-service, wager-producer, events-consumer"
