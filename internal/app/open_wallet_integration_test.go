//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres/pgtest"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

var ctx = context.Background()

// env is one isolated database with the use cases wired to it, connected as
// the application role, plus an owner pool for assertions.
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool // wallet_app, used by the use cases
	check *pgxpool.Pool // wallet_owner, used only to inspect rows
	tx    *postgres.TxManager
	open  *app.OpenWallet
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := pgtest.New(t)
	e := &env{t: t, pool: db.Pool(t, pgtest.App), check: db.Pool(t, pgtest.Owner)}
	e.tx = postgres.NewTxManager(e.pool)
	e.open = app.NewOpenWallet(e.tx, app.SystemClock{}, app.UUIDv7{})
	return e
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.check.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestOpenWalletWithBalance: wallet, OPENING, credit entry and both events are
// committed together, at version 1 (§9).
func TestOpenWalletWithBalance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	player := uuid.New()
	w, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: player, InitialBalance: brl(t, "1000.00"), CorrelationID: "corr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if w.PlayerID() != player || w.Balance() != brl(t, "1000.00") || w.Version() != 1 {
		t.Fatalf("wallet = %+v", w.Snapshot())
	}
	if n := e.count(`SELECT count(*) FROM wallets WHERE id = $1 AND balance_minor = 100000 AND version = 1`, w.ID()); n != 1 {
		t.Fatalf("stored wallets = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'
		AND origin = 'INTERNAL' AND status = 'PROCESSED' AND amount_minor = 100000 AND result_wallet_version = 1`, w.ID()); n != 1 {
		t.Fatalf("opening transactions = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'
		AND balance_before_minor = 0 AND balance_after_minor = 100000 AND wallet_version = 1`, w.ID()); n != 1 {
		t.Fatalf("opening entries = %d", n)
	}
	rows, err := e.check.Query(ctx, `SELECT event_id, event_type, aggregate_id, payload FROM outbox_events ORDER BY event_type DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var id, agg uuid.UUID
		var typ string
		var payload []byte
		if err := rows.Scan(&id, &typ, &agg, &payload); err != nil {
			t.Fatal(err)
		}
		var env struct {
			EventID       uuid.UUID `json:"eventId"`
			CorrelationID string    `json:"correlationId"`
		}
		if err := json.Unmarshal(payload, &env); err != nil || env.EventID != id || env.CorrelationID != "corr-1" {
			t.Fatalf("payload %s does not carry its own event id and correlation (%v)", payload, err)
		}
		if agg != w.ID() {
			t.Fatalf("aggregate %s, want wallet %s", agg, w.ID())
		}
		types = append(types, typ)
	}
	if len(types) != 2 || types[0] != "WalletBalanceChanged" || types[1] != "WagerTransactionProcessed" {
		t.Fatalf("outbox events = %v", types)
	}
}

// TestOpenWalletWithZeroBalance: no OPENING, ledger entry or event (§9).
func TestOpenWalletWithZeroBalance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), CorrelationID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || !w.Balance().IsZero() {
		t.Fatalf("wallet = %+v", w.Snapshot())
	}
	for _, table := range []string{"wager_transactions", "wallet_ledger_entries", "outbox_events"} {
		if n := e.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Errorf("%s has %d rows, want 0", table, n)
		}
	}
}

func TestOpenWalletRejectsDuplicatePlayerCurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	player := uuid.New()
	if _, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: player, InitialBalance: brl(t, "10.00"), CorrelationID: "c"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: player, InitialBalance: brl(t, "5.00"), CorrelationID: "c"})
	if !errors.Is(err, app.ErrWalletAlreadyExists) {
		t.Fatalf("err = %v, want ErrWalletAlreadyExists", err)
	}
	// The failed attempt left nothing behind: one wallet, one opening.
	if n := e.count(`SELECT count(*) FROM wager_transactions`); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
	usd, _ := money.Parse("5.00", "USD")
	if _, err := e.open.Execute(ctx, app.OpenWalletCommand{PlayerID: player, InitialBalance: usd, CorrelationID: "c"}); err != nil {
		t.Fatalf("another currency for the same player must be allowed: %v", err)
	}
}
