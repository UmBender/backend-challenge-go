//go:build e2e

// Package e2e runs black-box scenarios against the three replicas started by
// `docker compose up --build`: separate processes sharing only PostgreSQL,
// SQS and Keycloak. Correctness must hold whichever replica serves a request.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
)

var replicas = []string{"http://127.0.0.1:8081", "http://127.0.0.1:8082", "http://127.0.0.1:8083"}

var (
	tokensMu sync.Mutex
	tokens   = map[string]string{}
)

// token returns a real client_credentials token, cached for the run.
func token(t *testing.T, client string) string {
	t.Helper()
	tokensMu.Lock()
	defer tokensMu.Unlock()
	if tok, ok := tokens[client]; ok {
		return tok
	}
	resp, err := http.PostForm(os.Getenv("OIDC_ISSUER")+"/protocol/openid-connect/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {client + "-secret"},
	})
	if err != nil {
		t.Fatalf("token (is the stack up?): %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token for %s: %d %v", client, resp.StatusCode, err)
	}
	tokens[client] = body.AccessToken
	return body.AccessToken
}

type response struct {
	code int
	body map[string]any
}

// call sends one request; a transport error (replica down) is returned, not fatal.
func call(t *testing.T, base, client, method, path, body string, headers ...string) (response, error) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token(t, client))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := response{code: resp.StatusCode}
	_ = json.Unmarshal(raw, &r.body)
	return r, nil
}

func mustCall(t *testing.T, base, client, method, path, body string, headers ...string) response {
	t.Helper()
	r, err := call(t, base, client, method, path, body, headers...)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, base, path, err)
	}
	return r
}

type wallet struct{ id, player string }

func openWallet(t *testing.T, amount string) wallet {
	t.Helper()
	player := uuid.NewString()
	r := mustCall(t, replicas[0], "wallet-internal", http.MethodPost, "/wallets",
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, amount))
	if r.code != 201 {
		t.Fatalf("open wallet: %d %v", r.code, r.body)
	}
	return wallet{id: r.body["id"].(string), player: player}
}

func wagerBody(w wallet, kind, amount, ext, ref string) string {
	b := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,`+
		`"roundId":"round-1","gameId":"game-1","kind":%q,"money":{"amount":%q,"currency":"BRL"}`, ext, w.player, w.id, kind, amount)
	if ref != "" {
		b += fmt.Sprintf(`,"referenceExternalTransactionId":%q`, ref)
	}
	return b + "}"
}

func submit(t *testing.T, base string, w wallet, kind, amount, ext, ref string) (response, error) {
	return call(t, base, "provider-a", http.MethodPost, "/wagering/transactions", wagerBody(w, kind, amount, ext, ref),
		"Idempotency-Key", "provider-a:"+ext)
}

func balance(t *testing.T, w wallet) string {
	t.Helper()
	r := mustCall(t, replicas[2], "wallet-internal", http.MethodGet, "/wallets/"+w.id, "")
	return r.body["balance"].(map[string]any)["amount"].(string)
}

// assertReconciled is the final check of every scenario: the stored balance
// equals the balance rebuilt from the ledger.
func assertReconciled(t *testing.T, w wallet) {
	t.Helper()
	r := mustCall(t, replicas[1], "wallet-internal", http.MethodPost, "/wallets/"+w.id+"/reconciliation", "")
	if r.code != 200 || r.body["consistent"] != true {
		t.Fatalf("wallet %s not reconciled: %d %v", w.id, r.code, r.body)
	}
}

func ledgerSize(t *testing.T, w wallet) int {
	t.Helper()
	r := mustCall(t, replicas[0], "wallet-internal", http.MethodGet, "/wallets/"+w.id+"/ledger?limit=200", "")
	return len(r.body["items"].([]any))
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("OIDC_ISSUER") == "" {
		fmt.Fprintln(os.Stderr, "OIDC_ISSUER is not set; run through `make test-e2e`")
		os.Exit(1)
	}
	for _, base := range replicas {
		resp, err := http.Get(base + "/health/ready")
		if err != nil || resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "%s is not ready; start the stack with `docker compose up --build -d`\n", base)
			os.Exit(1)
		}
		resp.Body.Close()
	}
	os.Exit(m.Run())
}

// Two bets of 80 on a balance of 100, sent at once to different replicas:
// exactly one is processed and the balance never goes negative.
func TestConcurrentBetsOnDifferentReplicas(t *testing.T) {
	w := openWallet(t, "100.00")
	start := make(chan struct{})
	results := make([]response, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			<-start
			results[i] = mustSubmit(t, replicas[i], w, "BET", "80.00", fmt.Sprintf("bet-%s-%d", w.id[24:], i), "")
		})
	}
	close(start)
	wg.Wait()
	codes := map[int]int{}
	for _, r := range results {
		codes[r.code]++
	}
	if codes[201] != 1 || codes[422] != 1 {
		t.Fatalf("codes = %v, want one 201 and one 422", codes)
	}
	if got := balance(t, w); got != "20.00" {
		t.Fatalf("balance = %s, want 20.00", got)
	}
	assertReconciled(t, w)
}

func mustSubmit(t *testing.T, base string, w wallet, kind, amount, ext, ref string) response {
	t.Helper()
	r, err := submit(t, base, w, kind, amount, ext, ref)
	if err != nil {
		t.Errorf("request failed: %v", err)
	}
	return r
}

// The same bet sent 30 times concurrently, spread over the three replicas:
// one debit, every answer about the same transaction.
func TestSameBetOnAllReplicas(t *testing.T) {
	w := openWallet(t, "100.00")
	ext := "same-" + w.id[24:]
	start := make(chan struct{})
	var wg sync.WaitGroup
	var created atomic.Int32
	ids := sync.Map{}
	for i := range 30 {
		wg.Go(func() {
			<-start
			r := mustSubmit(t, replicas[i%3], w, "BET", "25.00", ext, "")
			if r.code == 201 {
				created.Add(1)
			} else if r.code != 200 {
				t.Errorf("unexpected %d %v", r.code, r.body)
			}
			ids.Store(r.body["transactionId"], true)
		})
	}
	close(start)
	wg.Wait()
	n := 0
	ids.Range(func(any, any) bool { n++; return true })
	if created.Load() != 1 || n != 1 {
		t.Fatalf("created = %d, distinct transactions = %d; want 1 and 1", created.Load(), n)
	}
	if got := balance(t, w); got != "75.00" {
		t.Fatalf("balance = %s, want 75.00", got)
	}
	if got := ledgerSize(t, w); got != 2 {
		t.Fatalf("ledger entries = %d, want 2 (opening, one debit)", got)
	}
	assertReconciled(t, w)
}

// Ten wallets receiving ten bets each, concurrently, through every replica.
func TestManyWalletsOnAllReplicas(t *testing.T) {
	var wallets []wallet
	for range 10 {
		wallets = append(wallets, openWallet(t, "100.00"))
	}
	var wg sync.WaitGroup
	for wi, w := range wallets {
		for b := range 10 {
			wg.Go(func() {
				r := mustSubmit(t, replicas[(wi+b)%3], w, "BET", "1.00", fmt.Sprintf("many-%s-%d", w.id[24:], b), "")
				if r.code != 201 {
					t.Errorf("bet: %d %v", r.code, r.body)
				}
			})
		}
	}
	wg.Wait()
	for _, w := range wallets {
		if got := balance(t, w); got != "90.00" {
			t.Fatalf("wallet %s balance = %s, want 90.00", w.id, got)
		}
		assertReconciled(t, w)
	}
}

// A REFUND sent before its BET is answered 202 by one replica and completed
// by the pending worker of whichever replica gets to it.
func TestRefundBeforeBetAcrossReplicas(t *testing.T) {
	w := openWallet(t, "100.00")
	bet, refund := "late-bet-"+w.id[24:], "early-refund-"+w.id[24:]
	r := mustSubmit(t, replicas[0], w, "REFUND", "30.00", refund, bet)
	if r.code != 202 {
		t.Fatalf("refund: %d %v", r.code, r.body)
	}
	if r := mustSubmit(t, replicas[1], w, "BET", "30.00", bet, ""); r.code != 201 {
		t.Fatalf("bet: %d %v", r.code, r.body)
	}
	eventually(t, "refund processed by a worker", func() bool {
		v := mustCall(t, replicas[2], "provider-a", http.MethodGet, "/providers/provider-a/wagering/transactions/"+refund, "")
		return v.body["status"] == "PROCESSED"
	})
	if got := balance(t, w); got != "100.00" {
		t.Fatalf("balance = %s, want 100.00", got)
	}
	assertReconciled(t, w)
}

// The same operation sent over HTTP and then over SQS has one effect.
func TestHTTPAndSQSShareIdempotency(t *testing.T) {
	w := openWallet(t, "100.00")
	ext := "dual-" + w.id[24:]
	if r := mustSubmit(t, replicas[0], w, "BET", "10.00", ext, ""); r.code != 201 {
		t.Fatalf("http bet: %d %v", r.code, r.body)
	}
	var data map[string]any
	_ = json.Unmarshal([]byte(wagerBody(w, "BET", "10.00", ext, "")), &data)
	data["idempotencyKey"] = "provider-a:" + ext
	msgID := "msg-" + ext
	body, _ := json.Marshal(map[string]any{"messageId": msgID, "type": "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": data})

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(os.Getenv("AWS_REGION")))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(os.Getenv("AWS_ENDPOINT_URL")) })
	q, err := client.GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions.fifo")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: q.QueueUrl, MessageBody: aws.String(string(body)),
		MessageGroupId: aws.String(w.id), MessageDeduplicationId: aws.String(msgID),
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the SQS copy consumed", func() bool {
		attrs, err := client.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{QueueUrl: q.QueueUrl,
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
		return err == nil && attrs.Attributes["ApproximateNumberOfMessages"] == "0" && attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
	})
	if got := balance(t, w); got != "90.00" {
		t.Fatalf("balance = %s, want 90.00 (one debit)", got)
	}
	if got := ledgerSize(t, w); got != 2 {
		t.Fatalf("ledger entries = %d, want 2", got)
	}
	assertReconciled(t, w)
}

// A replica is killed (SIGKILL) while bets flow through all three. Clients
// retry with the same Idempotency-Key on another replica, as the contract
// allows: every bet is applied exactly once and every wallet reconciles.
func TestReplicaKilledUnderLoad(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	var wallets []wallet
	for range 5 {
		wallets = append(wallets, openWallet(t, "100.00"))
	}
	var wg sync.WaitGroup
	var killed atomic.Bool
	for wi, w := range wallets {
		for b := range 20 {
			wg.Go(func() {
				if wi == 0 && b == 5 && killed.CompareAndSwap(false, true) {
					compose(t, "kill", "-s", "SIGKILL", "wallet-2")
				}
				ext := fmt.Sprintf("chaos-%s-%d", w.id[24:], b)
				for attempt := 0; ; attempt++ {
					r, err := submit(t, replicas[(b+attempt)%3], w, "BET", "1.00", ext, "")
					if err == nil && (r.code == 201 || r.code == 200) {
						return
					}
					if err == nil && r.code != 503 {
						t.Errorf("bet %s: %d %v", ext, r.code, r.body)
						return
					}
					if attempt > 20 {
						t.Errorf("bet %s never succeeded: %v", ext, err)
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			})
		}
	}
	wg.Wait()
	// The killed replica restarts (manual `docker kill` is not restarted by
	// the on-failure policy) and resumes outbox and pending work.
	compose(t, "start", "wallet-2")
	eventually(t, "wallet-2 back", func() bool {
		resp, err := http.Get(replicas[1] + "/health/ready")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	for _, w := range wallets {
		if got := balance(t, w); got != "80.00" {
			t.Fatalf("wallet %s balance = %s, want 80.00", w.id, got)
		}
		if got := ledgerSize(t, w); got != 21 {
			t.Fatalf("wallet %s ledger entries = %d, want 21", w.id, got)
		}
		assertReconciled(t, w)
	}
}

// compose runs docker compose from the repository root.
func compose(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("docker compose %v: %v %s", args, err, out)
	}
}

// waitReady blocks until every replica answers readiness.
func waitReady(t *testing.T) {
	t.Helper()
	for _, base := range replicas {
		eventually(t, base+" ready", func() bool {
			resp, err := http.Get(base + "/health/ready")
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == 200
		})
	}
}

// §13.8: every replica is killed abruptly (SIGKILL) and started again. A
// processed operation still replays its original result, and an operation
// left waiting for its reference is resumed by whichever replica gets to it.
func TestRestartPreservesIdempotencyAndPendings(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	w := openWallet(t, "100.00")
	bet, refund, late := "restart-bet-"+w.id[24:], "restart-refund-"+w.id[24:], "restart-late-"+w.id[24:]
	first := mustSubmit(t, replicas[0], w, "BET", "10.00", bet, "")
	if first.code != 201 {
		t.Fatalf("bet: %d %v", first.code, first.body)
	}
	if r := mustSubmit(t, replicas[1], w, "REFUND", "20.00", refund, late); r.code != 202 {
		t.Fatalf("refund: %d %v", r.code, r.body)
	}

	compose(t, "kill", "-s", "SIGKILL", "wallet-1", "wallet-2", "wallet-3")
	compose(t, "start", "wallet-1", "wallet-2", "wallet-3")
	waitReady(t)

	replay := mustSubmit(t, replicas[2], w, "BET", "10.00", bet, "")
	if replay.code != 200 || replay.body["idempotentReplay"] != true || replay.body["transactionId"] != first.body["transactionId"] ||
		replay.body["balance"].(map[string]any)["amount"] != "90.00" {
		t.Fatalf("replay after restart: %d %v", replay.code, replay.body)
	}
	v := mustCall(t, replicas[0], "provider-a", http.MethodGet, "/providers/provider-a/wagering/transactions/"+refund, "")
	if v.body["status"] != "PENDING_REFERENCE" {
		t.Fatalf("pending refund after restart: %v", v.body)
	}
	if r := mustSubmit(t, replicas[1], w, "BET", "20.00", late, ""); r.code != 201 {
		t.Fatalf("late bet: %d %v", r.code, r.body)
	}
	eventually(t, "pending refund resumed after restart", func() bool {
		v := mustCall(t, replicas[2], "provider-a", http.MethodGet, "/providers/provider-a/wagering/transactions/"+refund, "")
		return v.body["status"] == "PROCESSED"
	})
	if got := balance(t, w); got != "90.00" {
		t.Fatalf("balance = %s, want 90.00", got)
	}
	assertReconciled(t, w)
}

// After the scenarios, every committed event is eventually published.
func TestZZOutboxDrains(t *testing.T) {
	eventually(t, "outbox drained on every replica", func() bool {
		for _, base := range replicas {
			resp, err := http.Get(base + "/metrics")
			if err != nil {
				return false
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !bytes.Contains(raw, []byte("\noutbox_lag_seconds 0\n")) {
				return false
			}
		}
		return true
	})
}
