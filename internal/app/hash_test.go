package app

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

func sampleRequest(t *testing.T) wager.Request {
	t.Helper()
	m, err := money.Parse("25.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return wager.Request{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		PlayerID:              uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"),
		WalletID:              uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37"),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  wager.KindBet,
		Money:                 m,
	}
}

// TestPayloadHashCanonicalForm pins algorithm v1: SHA-256 of the business
// fields as JSON with sorted keys, no whitespace, no HTML escaping, the key
// and every transport field excluded. Changing this breaks every stored hash.
func TestPayloadHashCanonicalForm(t *testing.T) {
	canonical := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	want := sha256.Sum256([]byte(canonical))
	got, err := PayloadHash(sampleRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want[:]) {
		t.Fatalf("hash does not match the canonical form %s", canonical)
	}
}

func TestPayloadHashIncludesReferenceOnlyWhenPresent(t *testing.T) {
	r := sampleRequest(t)
	r.Kind, r.ReferenceExternalTransactionID = wager.KindRefund, "bet-1"
	canonical := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"REFUND",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","referenceExternalTransactionId":"bet-1","roundId":"round-987",` +
		`"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	want := sha256.Sum256([]byte(canonical))
	if got, _ := PayloadHash(r); string(got) != string(want[:]) {
		t.Fatalf("reference must be part of the canonical form when present")
	}
}

func TestPayloadHashDoesNotEscapeHTML(t *testing.T) {
	r := sampleRequest(t)
	r.GameID = "a<b&c>"
	canonical := `{"externalTransactionId":"transaction-123","gameId":"a<b&c>","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	want := sha256.Sum256([]byte(canonical))
	if got, _ := PayloadHash(r); string(got) != string(want[:]) {
		t.Fatal("HTML characters must be hashed as sent, not as \\u003c escapes")
	}
}

func TestEveryBusinessFieldChangesTheHash(t *testing.T) {
	base, _ := PayloadHash(sampleRequest(t))
	other, _ := money.Parse("25.01", "BRL")
	usd, _ := money.Parse("25.00", "USD")
	for name, change := range map[string]func(*wager.Request){
		"provider":    func(r *wager.Request) { r.ProviderID = "provider-b" },
		"external id": func(r *wager.Request) { r.ExternalTransactionID = "x" },
		"player":      func(r *wager.Request) { r.PlayerID = uuid.New() },
		"wallet":      func(r *wager.Request) { r.WalletID = uuid.New() },
		"round":       func(r *wager.Request) { r.RoundID = "x" },
		"game":        func(r *wager.Request) { r.GameID = "x" },
		"kind":        func(r *wager.Request) { r.Kind = wager.KindWin },
		"amount":      func(r *wager.Request) { r.Money = other },
		"currency":    func(r *wager.Request) { r.Money = usd },
		"reference":   func(r *wager.Request) { r.ReferenceExternalTransactionID = "x" },
	} {
		r := sampleRequest(t)
		change(&r)
		if h, _ := PayloadHash(r); string(h) == string(base) {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}
