package wager

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func validBet(t *testing.T) Request {
	return Request{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		PlayerID:              uuid.New(),
		WalletID:              uuid.New(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  KindBet,
		Money:                 brl(t, "25.00"),
	}
}

func TestParseExternalKind(t *testing.T) {
	for _, s := range []string{"BET", "WIN", "LOSS", "REFUND", "ROLLBACK"} {
		if k, err := ParseExternalKind(s); err != nil || string(k) != s {
			t.Errorf("ParseExternalKind(%q) = %q, %v", s, k, err)
		}
	}
	// OPENING is internal only (§6.3): a distinct, stable code.
	assertInvalid(t, func() error { _, err := ParseExternalKind("OPENING"); return err }(), CodeUnsupportedKind)
	for _, s := range []string{"", "bet", "DEPOSIT"} {
		assertInvalid(t, func() error { _, err := ParseExternalKind(s); return err }(), CodeInvalidRequest)
	}
}

func TestValidRequestsPass(t *testing.T) {
	for name, change := range map[string]func(*Request){
		"bet": func(*Request) {},
		"win": func(r *Request) { r.Kind = KindWin },
		"win with reference": func(r *Request) {
			r.Kind, r.ReferenceExternalTransactionID = KindWin, "bet-1"
		},
		"loss with zero": func(r *Request) { r.Kind, r.Money = KindLoss, brl(t, "0.00") },
		"refund": func(r *Request) {
			r.Kind, r.ReferenceExternalTransactionID = KindRefund, "bet-1"
		},
		"rollback": func(r *Request) {
			r.Kind, r.ReferenceExternalTransactionID = KindRollback, "bet-1"
		},
	} {
		r := validBet(t)
		change(&r)
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidRequests(t *testing.T) {
	long := strings.Repeat("x", 256)
	for name, tt := range map[string]struct {
		change func(*Request)
		code   FailureCode
	}{
		"no provider":          {func(r *Request) { r.ProviderID = "" }, CodeInvalidRequest},
		"no external id":       {func(r *Request) { r.ExternalTransactionID = "" }, CodeInvalidRequest},
		"external id too long": {func(r *Request) { r.ExternalTransactionID = long }, CodeInvalidRequest},
		"no round":             {func(r *Request) { r.RoundID = "" }, CodeInvalidRequest},
		"no game":              {func(r *Request) { r.GameID = "" }, CodeInvalidRequest},
		"nil player":           {func(r *Request) { r.PlayerID = uuid.Nil }, CodeInvalidRequest},
		"nil wallet":           {func(r *Request) { r.WalletID = uuid.Nil }, CodeInvalidRequest},
		"opening":              {func(r *Request) { r.Kind = KindOpening }, CodeUnsupportedKind},
		"unknown kind":         {func(r *Request) { r.Kind = "JACKPOT" }, CodeInvalidRequest},
		"missing money":        {func(r *Request) { r.Money = money.Money{} }, CodeInvalidMoney},
		// Zero policy (§7): zero only for LOSS (and internal openings).
		"bet zero":  {func(r *Request) { r.Money = brl(t, "0.00") }, CodeInvalidAmountForKind},
		"win zero":  {func(r *Request) { r.Kind, r.Money = KindWin, brl(t, "0.00") }, CodeInvalidAmountForKind},
		"loss 1.00": {func(r *Request) { r.Kind = KindLoss }, CodeInvalidAmountForKind},
		"refund zero": {func(r *Request) {
			r.Kind, r.Money, r.ReferenceExternalTransactionID = KindRefund, brl(t, "0.00"), "bet-1"
		}, CodeInvalidAmountForKind},
		"rollback zero": {func(r *Request) {
			r.Kind, r.Money, r.ReferenceExternalTransactionID = KindRollback, brl(t, "0.00"), "bet-1"
		}, CodeInvalidAmountForKind},
		// References.
		"refund without reference":   {func(r *Request) { r.Kind = KindRefund }, CodeInvalidRequest},
		"rollback without reference": {func(r *Request) { r.Kind = KindRollback }, CodeInvalidRequest},
		"bet with reference":         {func(r *Request) { r.ReferenceExternalTransactionID = "x" }, CodeInvalidRequest},
		"loss with reference": {func(r *Request) {
			r.Kind, r.Money, r.ReferenceExternalTransactionID = KindLoss, brl(t, "0.00"), "x"
		}, CodeInvalidRequest},
		"self reference": {func(r *Request) {
			r.Kind, r.ReferenceExternalTransactionID = KindRefund, r.ExternalTransactionID
		}, CodeInvalidRequest},
	} {
		r := validBet(t)
		tt.change(&r)
		err := r.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		var inv *InvalidRequestError
		if !errors.As(err, &inv) || inv.Code != tt.code {
			t.Errorf("%s: err = %v, want code %s", name, err, tt.code)
		}
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	if err := ValidateIdempotencyKey("provider-a:transaction-123"); err != nil {
		t.Fatal(err)
	}
	assertInvalid(t, ValidateIdempotencyKey(""), CodeMissingIdempotencyKey)
	assertInvalid(t, ValidateIdempotencyKey(strings.Repeat("k", 256)), CodeInvalidRequest)
}

func assertInvalid(t *testing.T, err error, code FailureCode) {
	t.Helper()
	var inv *InvalidRequestError
	if !errors.As(err, &inv) || inv.Code != code {
		t.Errorf("err = %v, want InvalidRequestError %s", err, code)
	}
}
