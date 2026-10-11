package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

const defaultLedgerLimit = 50

type walletBody struct {
	ID       uuid.UUID   `json:"id"`
	PlayerID uuid.UUID   `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

type walletView struct {
	walletBody
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func newWalletBody(w *wallet.Wallet) walletBody {
	return walletBody{ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version()}
}

func (a *API) openWallet(w http.ResponseWriter, r *http.Request) {
	cmd, err := decodeOpenWallet(w, r)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	cmd.CorrelationID = correlationID(r.Context())
	wal, err := a.Opener.Execute(r.Context(), cmd)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+wal.ID().String())
	writeJSON(w, http.StatusCreated, newWalletBody(wal))
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	wal, err := a.Wallets.Wallet(r.Context(), id)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, walletView{walletBody: newWalletBody(wal), CreatedAt: wal.CreatedAt(), UpdatedAt: wal.UpdatedAt()})
}

type ledgerItem struct {
	ID            uuid.UUID        `json:"id"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     ledger.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
	CreatedAt     time.Time        `json:"createdAt"`
}

type ledgerBody struct {
	Items      []ledgerItem `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

func (a *API) getLedger(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	q := r.URL.Query()
	limit := defaultLedgerLimit
	if s := q.Get("limit"); s != "" {
		if limit, err = strconv.Atoi(s); err != nil {
			writeError(w, r, a.Log, fmt.Errorf("%w: limit must be an integer", app.ErrInvalidPage))
			return
		}
	}
	var after int64
	if s := q.Get("cursor"); s != "" {
		if after, err = decodeCursor(s); err != nil {
			writeError(w, r, a.Log, err)
			return
		}
	}
	page, err := a.Wallets.Ledger(r.Context(), id, after, limit)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	body := ledgerBody{Items: make([]ledgerItem, 0, len(page.Records))}
	for _, rec := range page.Records {
		e := rec.Entry
		body.Items = append(body.Items, ledgerItem{
			ID: e.ID(), TransactionID: e.TransactionID(), Direction: e.Direction(), Money: e.Amount(),
			BalanceBefore: e.Before(), BalanceAfter: e.After(), WalletVersion: e.WalletVersion(), CreatedAt: e.CreatedAt(),
		})
	}
	if page.HasMore {
		body.NextCursor = encodeCursor(page.NextAfter)
	}
	writeJSON(w, http.StatusOK, body)
}

// cursor is opaque (base64url JSON) so clients treat it as a token. It is
// not signed: a forged cursor only moves the caller within a ledger it may
// already read.
type cursor struct {
	After int64 `json:"after"`
}

func encodeCursor(after int64) string {
	b, _ := json.Marshal(cursor{After: after})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("%w: malformed cursor", app.ErrInvalidPage)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c cursor
	if err := dec.Decode(&c); err != nil || c.After < 1 {
		return 0, fmt.Errorf("%w: malformed cursor", app.ErrInvalidPage)
	}
	return c.After, nil
}

type reconciliationBody struct {
	WalletID          uuid.UUID   `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	rec, err := a.Reconciler.Execute(r.Context(), id)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationBody{
		WalletID: rec.WalletID, StoredBalance: rec.Stored, CalculatedBalance: rec.Calculated,
		Difference: rec.Difference, Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}
