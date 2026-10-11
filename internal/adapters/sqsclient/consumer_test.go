package sqsclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

const validMessage = `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",
"data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

func TestParseMessageBuildsTheSharedCommand(t *testing.T) {
	cmd, err := parseMessage([]byte(validMessage))
	if err != nil {
		t.Fatal(err)
	}
	r := cmd.Request
	if r.ProviderID != "provider-a" || r.Kind != wager.KindBet || r.Money.String() != "25.00" ||
		cmd.IdempotencyKey != "provider-a:transaction-123" || cmd.Inbox.MessageID != "msg-123" || cmd.Inbox.Consumer != consumerName {
		t.Fatalf("command = %+v", cmd)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("the example message must be a valid request: %v", err)
	}
}

func TestParseMessageRejectsMalformedInputPermanently(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{`,
		"no messageId":   strings.Replace(validMessage, `"messageId":"msg-123"`, `"messageId":""`, 1),
		"wrong type":     strings.Replace(validMessage, `WagerTransactionRequested`, `Other`, 1),
		"unknown field":  strings.Replace(validMessage, `"kind":"BET"`, `"kind":"BET","extra":1`, 1),
		"numeric amount": strings.Replace(validMessage, `"amount":"25.00"`, `"amount":25.00`, 1),
		"upper uuid":     strings.Replace(validMessage, `0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1`, `0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1`, 1),
		"trailing data":  validMessage + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMessage([]byte(body)); err == nil || !permanent(err) {
				t.Fatalf("err = %v, want a permanent error", err)
			}
		})
	}
}

func TestErrorClassification(t *testing.T) {
	for _, err := range []error{app.ErrIdempotencyKeyReused, app.ErrMessageReused, app.ErrWalletNotFound, app.ErrInvariantViolation} {
		if !permanent(err) {
			t.Errorf("%v must go to the DLQ", err)
		}
	}
	for _, err := range []error{app.ErrUnavailable, app.ErrConcurrentConflict, errors.New("unknown")} {
		if permanent(err) {
			t.Errorf("%v must be retried", err)
		}
	}
}
