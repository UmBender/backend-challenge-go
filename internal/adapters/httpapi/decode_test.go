package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

const validSubmit = `{
  "providerId": "provider-a",
  "externalTransactionId": "transaction-123",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "roundId": "round-987",
  "gameId": "fortune-chimp",
  "kind": "REFUND",
  "money": { "amount": "25.00", "currency": "BRL" },
  "referenceExternalTransactionId": "bet-1"
}`

func jsonRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDecodeSubmitRequest(t *testing.T) {
	req, err := decodeSubmit(httptest.NewRecorder(), jsonRequest(validSubmit))
	if err != nil {
		t.Fatal(err)
	}
	if req.ProviderID != "provider-a" || req.ExternalTransactionID != "transaction-123" ||
		req.PlayerID != uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1") ||
		req.WalletID != uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37") ||
		req.RoundID != "round-987" || req.GameID != "fortune-chimp" || req.Kind != wager.KindRefund ||
		req.Money.String() != "25.00" || req.Money.Currency().String() != "BRL" ||
		req.ReferenceExternalTransactionID != "bet-1" {
		t.Fatalf("decoded %+v", req)
	}
}

// TestDecodeIsStrict: anything ambiguous is a 400, never silently accepted,
// because what is accepted is what gets hashed for idempotency.
func TestDecodeIsStrict(t *testing.T) {
	for name, tt := range map[string]struct {
		body   string
		ctype  string
		status int
		code   string
	}{
		"unknown field":       {strings.Replace(validSubmit, `"gameId"`, `"extra": 1, "gameId"`, 1), "", 400, "INVALID_REQUEST"},
		"different key case":  {strings.Replace(validSubmit, `"providerId"`, `"ProviderId"`, 1), "", 400, "INVALID_REQUEST"},
		"duplicate key":       {strings.Replace(validSubmit, `"gameId"`, `"kind": "BET", "gameId"`, 1), "", 400, "INVALID_REQUEST"},
		"two objects":         {validSubmit + `{}`, "", 400, "INVALID_REQUEST"},
		"array":               {`[]`, "", 400, "INVALID_REQUEST"},
		"empty body":          {``, "", 400, "INVALID_REQUEST"},
		"null":                {`null`, "", 400, "INVALID_REQUEST"},
		"malformed":           {`{"providerId":`, "", 400, "INVALID_REQUEST"},
		"number for a string": {strings.Replace(validSubmit, `"round-987"`, `987`, 1), "", 400, "INVALID_REQUEST"},
		"numeric amount":      {strings.Replace(validSubmit, `"25.00"`, `25.00`, 1), "", 400, "INVALID_MONEY"},
		"amount scale":        {strings.Replace(validSubmit, `"25.00"`, `"25"`, 1), "", 400, "INVALID_MONEY"},
		"bad uuid":            {strings.Replace(validSubmit, `0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1`, `player-1`, 1), "", 400, "INVALID_REQUEST"},
		"upper-case uuid":     {strings.Replace(validSubmit, `0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1`, `0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1`, 1), "", 400, "INVALID_REQUEST"},
		"braced uuid":         {strings.Replace(validSubmit, `"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"`, `"{0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1}"`, 1), "", 400, "INVALID_REQUEST"},
		"too large":           {`{"gameId":"` + strings.Repeat("x", 70<<10) + `"}`, "", 413, "PAYLOAD_TOO_LARGE"},
		"not json":            {validSubmit, "text/plain", 415, "UNSUPPORTED_MEDIA_TYPE"},
	} {
		r := jsonRequest(tt.body)
		if tt.ctype != "" {
			r.Header.Set("Content-Type", tt.ctype)
		}
		rec := httptest.NewRecorder()
		_, err := decodeSubmit(rec, r)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		writeError(rec, r, discard, err)
		if rec.Code != tt.status {
			t.Errorf("%s: status %d, want %d (%v)", name, rec.Code, tt.status, err)
			continue
		}
		if p := decodeProblem(t, rec); p.Code != tt.code {
			t.Errorf("%s: code %s, want %s", name, p.Code, tt.code)
		}
	}
}

func TestDecodeAcceptsJSONWithCharset(t *testing.T) {
	r := jsonRequest(validSubmit)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if _, err := decodeSubmit(httptest.NewRecorder(), r); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeOpenWallet(t *testing.T) {
	r := jsonRequest(`{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}`)
	cmd, err := decodeOpenWallet(httptest.NewRecorder(), r)
	if err != nil || cmd.InitialBalance.String() != "1000.00" || cmd.PlayerID.String() != "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1" {
		t.Fatalf("cmd = %+v, %v", cmd, err)
	}
	r = jsonRequest(`{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"}`)
	if _, err := decodeOpenWallet(httptest.NewRecorder(), r); err == nil {
		t.Fatal("missing initialBalance accepted")
	}
}
