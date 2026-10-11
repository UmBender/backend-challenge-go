package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

type fakeWallets struct {
	w         *wallet.Wallet
	openErr   error
	opened    app.OpenWalletCommand
	page      app.LedgerPage
	gotAfter  int64
	gotLimit  int
	reconcile app.Reconciliation
}

func (f *fakeWallets) Execute(_ context.Context, cmd app.OpenWalletCommand) (*wallet.Wallet, error) {
	f.opened = cmd
	return f.w, f.openErr
}

func (f *fakeWallets) Wallet(_ context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	if f.w == nil || f.w.ID() != id {
		return nil, app.ErrWalletNotFound
	}
	return f.w, nil
}

func (f *fakeWallets) Ledger(_ context.Context, id uuid.UUID, after int64, limit int) (app.LedgerPage, error) {
	f.gotAfter, f.gotLimit = after, limit
	if limit < 1 || limit > app.MaxLedgerPage {
		return app.LedgerPage{}, app.ErrInvalidPage
	}
	return f.page, nil
}

type fakeReconciler struct{ res app.Reconciliation }

func (f fakeReconciler) Execute(context.Context, uuid.UUID) (app.Reconciliation, error) {
	return f.res, nil
}

func m(t *testing.T, s string) money.Money {
	t.Helper()
	v, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func walletAPI(t *testing.T, f *fakeWallets) *API {
	return &API{Submit: &fakeSubmitter{}, Reader: &fakeReader{}, Opener: f, Wallets: f, Reconciler: fakeReconciler{res: f.reconcile}}
}

func TestOpenWalletEndpoint(t *testing.T) {
	w, _ := wallet.New(uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37"), uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"), m(t, "1000.00"), time.Now())
	f := &fakeWallets{w: w}
	rec := serve(t, walletAPI(t, f), internal, http.MethodPost, "/wallets",
		`{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}`)
	if rec.Code != 201 || rec.Header().Get("Location") != "/wallets/"+w.ID().String() {
		t.Fatalf("status %d location %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	want := `{"id":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","balance":{"amount":"1000.00","currency":"BRL"},"version":1}`
	if got := rec.Body.String(); got != want+"\n" {
		t.Fatalf("body %s, want %s (the §9 example)", got, want)
	}
	if f.opened.InitialBalance != m(t, "1000.00") {
		t.Fatal("command not passed")
	}

	f.openErr = app.ErrWalletAlreadyExists
	if rec := serve(t, walletAPI(t, f), internal, http.MethodPost, "/wallets",
		`{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1.00","currency":"BRL"}}`); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
}

// TestWalletEndpointsAreInternalOnly (§2): providers cannot touch wallets.
func TestWalletEndpointsAreInternalOnly(t *testing.T) {
	f := &fakeWallets{}
	id := uuid.NewString()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/wallets", `{}`},
		{http.MethodGet, "/wallets/" + id, ""},
		{http.MethodGet, "/wallets/" + id + "/ledger", ""},
		{http.MethodPost, "/wallets/" + id + "/reconciliation", ""},
	} {
		if rec := serve(t, walletAPI(t, f), providerA, c.method, c.path, c.body); rec.Code != 403 {
			t.Errorf("%s %s as a provider: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestGetWallet(t *testing.T) {
	w, _ := wallet.New(uuid.New(), uuid.New(), m(t, "10.00"), time.Now())
	f := &fakeWallets{w: w}
	rec := serve(t, walletAPI(t, f), internal, http.MethodGet, "/wallets/"+w.ID().String(), "")
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["id"] != w.ID().String() || body["version"] != float64(1) || body["createdAt"] == nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if rec := serve(t, walletAPI(t, f), internal, http.MethodGet, "/wallets/"+uuid.NewString(), ""); rec.Code != 404 {
		t.Fatalf("unknown wallet: %d", rec.Code)
	}
}

func TestLedgerEndpointUsesAnOpaqueCursor(t *testing.T) {
	w, _ := wallet.New(uuid.New(), uuid.New(), m(t, "10.00"), time.Now())
	zero, _ := money.Zero(money.BRL)
	e, err := ledger.NewEntry(ledger.Params{ID: uuid.New(), WalletID: w.ID(), TransactionID: uuid.New(), Direction: ledger.Credit,
		Amount: m(t, "10.00"), Before: zero, After: m(t, "10.00"), WalletVersion: 1, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWallets{w: w, page: app.LedgerPage{Records: []app.LedgerRecord{{Seq: 41, Entry: e}}, NextAfter: 41, HasMore: true}}
	api := walletAPI(t, f)
	base := "/wallets/" + w.ID().String() + "/ledger"

	rec := serve(t, api, internal, http.MethodGet, base, "")
	var body struct {
		Items []struct {
			ID            string                  `json:"id"`
			TransactionID string                  `json:"transactionId"`
			Direction     string                  `json:"direction"`
			Money         struct{ Amount string } `json:"money"`
			BalanceAfter  struct{ Amount string } `json:"balanceAfter"`
			WalletVersion int64                   `json:"walletVersion"`
		} `json:"items"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if f.gotAfter != 0 || f.gotLimit != 50 {
		t.Fatalf("first page asked after=%d limit=%d, want 0 and the default 50", f.gotAfter, f.gotLimit)
	}
	if len(body.Items) != 1 || body.Items[0].Direction != "CREDIT" || body.Items[0].Money.Amount != "10.00" ||
		body.Items[0].BalanceAfter.Amount != "10.00" || body.Items[0].WalletVersion != 1 || body.NextCursor == "" {
		t.Fatalf("body = %+v", body)
	}
	// The cursor is opaque: no raw sequence number in the URL.
	if body.NextCursor == "41" {
		t.Fatal("cursor is not opaque")
	}
	serve(t, api, internal, http.MethodGet, base+"?limit=10&cursor="+body.NextCursor, "")
	if f.gotAfter != 41 || f.gotLimit != 10 {
		t.Fatalf("second page asked after=%d limit=%d", f.gotAfter, f.gotLimit)
	}
	for _, q := range []string{"?cursor=not-base64!", "?cursor=eyJ4IjoxfQ", "?limit=abc", "?limit=0", "?limit=1000"} {
		if rec := serve(t, api, internal, http.MethodGet, base+q, ""); rec.Code != 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	f.page.HasMore = false
	rec = serve(t, api, internal, http.MethodGet, base, "")
	var last map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &last); last["nextCursor"] != nil {
		t.Fatal("last page must not carry a cursor")
	}
}

func TestReconciliationEndpoint(t *testing.T) {
	id := uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	zero, _ := money.Zero(money.BRL)
	f := &fakeWallets{reconcile: app.Reconciliation{WalletID: id, Stored: m(t, "975.00"), Calculated: m(t, "975.00"),
		Difference: zero, Consistent: true, CheckedEntries: 2}}
	rec := serve(t, walletAPI(t, f), internal, http.MethodPost, "/wallets/"+id.String()+"/reconciliation", "")
	want := `{"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","storedBalance":{"amount":"975.00","currency":"BRL"},` +
		`"calculatedBalance":{"amount":"975.00","currency":"BRL"},"difference":{"amount":"0.00","currency":"BRL"},` +
		`"consistent":true,"checkedEntries":2}`
	if rec.Code != 200 || rec.Body.String() != want+"\n" {
		t.Fatalf("status %d body %s, want the §9 example", rec.Code, rec.Body)
	}
}
