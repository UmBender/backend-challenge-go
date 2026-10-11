package httpapi

import (
	"context"

	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

const idempotencyHeader = "Idempotency-Key"

// The balance appears only for processed operations: a rejection has no
// meaningful balance after, and a pending one has none yet.
type submitResponse struct {
	TransactionID    uuid.UUID    `json:"transactionId"`
	Status           wager.Status `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	req, err := decodeSubmit(w, r)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	// The provider is the token's, never the body's (§2). An empty providerId
	// is left to validation (400).
	if req.ProviderID != "" && req.ProviderID != p.ProviderID {
		writeError(w, r, a.Log, errProviderMismatch)
		return
	}
	key := r.Header.Get(idempotencyHeader)
	ctx := r.Context()
	correlation := correlationID(ctx)
	if !validCorrelation(r.Header.Get(correlationHeader)) && validCorrelation(key) {
		// Without a client correlation id, the idempotency key is the natural one.
		correlation = key
		w.Header().Set(correlationHeader, key)
		ctx = context.WithValue(ctx, correlationKey, key)
		r = r.WithContext(ctx)
	}
	res, err := a.Submit.Execute(ctx, app.SubmitCommand{
		Request: req, IdempotencyKey: key, CorrelationID: correlation, CausationID: requestID(ctx),
	})
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	tx := res.Transaction
	body := submitResponse{TransactionID: tx.ID(), Status: tx.Status(), IdempotentReplay: res.Replay}
	var status int
	switch tx.Status() {
	case wager.StatusProcessed:
		b := tx.ResultBalance() // the balance observed then, also on replay
		body.Balance = &b
		status = http.StatusCreated
		if res.Replay {
			status = http.StatusOK
		}
	case wager.StatusRejected:
		body.FailureCode = string(tx.FailureCode())
		status = http.StatusUnprocessableEntity
	case wager.StatusFailed:
		// Only the pending worker records FAILED; a replay reports it.
		body.FailureCode = string(tx.FailureCode())
		status = http.StatusInternalServerError
	default: // PENDING_REFERENCE (and PENDING, which this path never commits)
		w.Header().Set("Location", "/wagering/transactions/"+tx.ID().String())
		status = http.StatusAccepted
	}
	writeJSON(w, status, body)
}

func (a *API) getTransaction(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := parseID(r.PathValue("transactionId"))
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	tx, err := a.Reader.Transaction(r.Context(), id)
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	// Not found rather than forbidden: a 403 would confirm the id exists.
	if !p.Has(RoleWalletAdmin) && tx.ProviderID() != p.ProviderID {
		writeError(w, r, a.Log, app.ErrTransactionNotFound)
		return
	}
	writeJSON(w, http.StatusOK, newTransactionView(tx))
}

func (a *API) getByExternalID(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	provider := r.PathValue("providerId")
	// Another provider's namespace is refused whatever exists in it, so the
	// answer leaks nothing.
	if !p.Has(RoleWalletAdmin) && provider != p.ProviderID {
		writeError(w, r, a.Log, errProviderMismatch)
		return
	}
	tx, err := a.Reader.TransactionByExternalID(r.Context(), provider, r.PathValue("externalTransactionId"))
	if err != nil {
		writeError(w, r, a.Log, err)
		return
	}
	writeJSON(w, http.StatusOK, newTransactionView(tx))
}

func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("%w: invalid id %q", errInvalidBody, s)
	}
	return id, nil
}

type transactionView struct {
	TransactionID                  uuid.UUID    `json:"transactionId"`
	Kind                           wager.Kind   `json:"kind"`
	Status                         wager.Status `json:"status"`
	WalletID                       uuid.UUID    `json:"walletId"`
	PlayerID                       uuid.UUID    `json:"playerId"`
	Money                          money.Money  `json:"money"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         *uuid.UUID   `json:"referenceTransactionId,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	WalletVersion                  *int64       `json:"walletVersion,omitempty"`
	Attempts                       int          `json:"attempts,omitempty"`
	NextAttemptAt                  *time.Time   `json:"nextAttemptAt,omitempty"`
	ExpiresAt                      *time.Time   `json:"expiresAt,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
	ProcessedAt                    *time.Time   `json:"processedAt,omitempty"`
}

func newTransactionView(tx *wager.Transaction) transactionView {
	s := tx.Snapshot()
	v := transactionView{
		TransactionID: s.ID, Kind: s.Kind, Status: s.Status, WalletID: s.WalletID, PlayerID: s.PlayerID,
		Money: s.Money, ProviderID: s.ProviderID, ExternalTransactionID: s.ExternalTransactionID,
		RoundID: s.RoundID, GameID: s.GameID, ReferenceExternalTransactionID: s.ReferenceExternalTransactionID,
		FailureCode: string(s.FailureCode), Attempts: s.Attempts, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		NextAttemptAt: optTime(s.NextAttemptAt), ExpiresAt: optTime(s.ExpiresAt), ProcessedAt: optTime(s.ProcessedAt),
	}
	if s.ReferenceTransactionID != uuid.Nil {
		v.ReferenceTransactionID = &s.ReferenceTransactionID
	}
	if s.ResultBalance.IsValid() {
		v.Balance, v.WalletVersion = &s.ResultBalance, &s.ResultWalletVersion
	}
	return v
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
