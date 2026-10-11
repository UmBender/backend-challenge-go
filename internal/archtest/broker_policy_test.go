package archtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// LocalStack Community stores IAM and queue policies but does not enforce
// them, so the least-privilege matrix (§2) is checked here, against the same
// files the broker is provisioned with, by a minimal IAM evaluator: an
// explicit Deny wins, otherwise any matching Allow grants.

const (
	acct     = "arn:aws:sqs:us-east-1:000000000000:"
	inQueue  = acct + "wager-transactions.fifo"
	dlqQueue = acct + "wager-transactions-dlq.fifo"
	evQueue  = acct + "wallet-events.fifo"
	userARN  = "arn:aws:iam::000000000000:user/"
)

type policy struct {
	Statement []struct {
		Effect    string
		Principal struct{ AWS stringList }
		Action    stringList
		Resource  stringList
	}
}

// stringList accepts both "x" and ["x", "y"], as IAM does.
type stringList []string

func (s *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	err := json.Unmarshal(b, &many)
	*s = many
	return err
}

func loadPolicy(t *testing.T, name string) policy {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "localstack", "iam", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var p policy
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

func glob(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(s)
}

// allows evaluates action on resource; principal is checked only when the
// statement names one (resource policies).
func (p policy) allows(principal, action, resource string) bool {
	allowed := false
	for _, s := range p.Statement {
		if len(s.Principal.AWS) > 0 && !slices.ContainsFunc(s.Principal.AWS, func(pr string) bool { return glob(pr, userARN+principal) }) {
			continue
		}
		if !slices.ContainsFunc(s.Action, func(a string) bool { return glob(a, action) }) ||
			!slices.ContainsFunc(s.Resource, func(r string) bool { return glob(r, resource) }) {
			continue
		}
		if s.Effect == "Deny" {
			return false
		}
		allowed = allowed || s.Effect == "Allow"
	}
	return allowed
}

type grant struct{ action, resource string }

var everyQueue = []string{inQueue, dlqQueue, evQueue}

var everyAction = []string{
	"sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility",
	"sqs:GetQueueUrl", "sqs:GetQueueAttributes", "sqs:PurgeQueue", "sqs:DeleteQueue", "sqs:SetQueueAttributes",
}

// The complete set of what each identity may do; everything else is denied.
var expected = map[string][]grant{
	"wallet-service": {
		{"sqs:GetQueueUrl", inQueue}, {"sqs:GetQueueUrl", dlqQueue}, {"sqs:GetQueueUrl", evQueue},
		{"sqs:GetQueueAttributes", inQueue}, {"sqs:GetQueueAttributes", dlqQueue}, {"sqs:GetQueueAttributes", evQueue},
		{"sqs:ReceiveMessage", inQueue}, {"sqs:DeleteMessage", inQueue}, {"sqs:ChangeMessageVisibility", inQueue},
		{"sqs:SendMessage", dlqQueue}, {"sqs:SendMessage", evQueue},
	},
	"wager-producer": {
		{"sqs:SendMessage", inQueue}, {"sqs:GetQueueUrl", inQueue},
	},
	"events-consumer": {
		{"sqs:GetQueueUrl", evQueue}, {"sqs:ReceiveMessage", evQueue},
		{"sqs:DeleteMessage", evQueue}, {"sqs:ChangeMessageVisibility", evQueue},
	},
}

func TestIdentityPoliciesAreLeastPrivilege(t *testing.T) {
	for identity, grants := range expected {
		p := loadPolicy(t, identity)
		for _, action := range everyAction {
			for _, queue := range everyQueue {
				want := slices.Contains(grants, grant{action, queue})
				if got := p.allows(identity, action, queue); got != want {
					t.Errorf("%s %s on %s: allowed = %v, want %v", identity, action, queue, got, want)
				}
			}
		}
	}
}

// Queue policies are a second, resource-side layer: a principal is allowed
// only what its identity policy grants on that queue (GetQueueUrl is an
// identity-side permission only).
func TestQueuePoliciesAdmitOnlyTheExpectedPrincipals(t *testing.T) {
	queues := map[string]string{
		"queue-wager-transactions": inQueue, "queue-wager-transactions-dlq": dlqQueue, "queue-wallet-events": evQueue,
	}
	for file, queue := range queues {
		p := loadPolicy(t, file)
		for identity, grants := range expected {
			for _, action := range everyAction {
				want := action != "sqs:GetQueueUrl" && slices.Contains(grants, grant{action, queue})
				if got := p.allows(identity, action, queue); got != want {
					t.Errorf("%s: %s %s: allowed = %v, want %v", file, identity, action, got, want)
				}
			}
		}
		if p.allows("someone-else", "sqs:SendMessage", queue) || p.allows("someone-else", "sqs:ReceiveMessage", queue) {
			t.Errorf("%s admits an unknown principal", file)
		}
	}
}

func TestPolicyEvaluatorHonoursDenyAndWildcards(t *testing.T) {
	p := policy{}
	_ = json.Unmarshal([]byte(`{"Statement":[
		{"Effect":"Allow","Action":"sqs:*","Resource":"arn:aws:sqs:us-east-1:000000000000:*"},
		{"Effect":"Deny","Action":"sqs:PurgeQueue","Resource":"*"}]}`), &p)
	if !p.allows("x", "sqs:SendMessage", inQueue) || p.allows("x", "sqs:PurgeQueue", inQueue) {
		t.Fatal("evaluator must apply wildcards and let an explicit Deny win")
	}
}
