//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres/pgtest"
)

// SQLSTATE codes asserted by the schema tests. The codes, not the messages,
// are the contract the persistence adapter maps to application errors (T4).
const (
	uniqueViolation       = "23505"
	checkViolation        = "23514"
	foreignKeyViolation   = "23503"
	notNullViolation      = "23502"
	restrictViolation     = "23001"
	insufficientPrivilege = "42501"
)

var ctx = context.Background()

// schemaTables is every table the migrations create.
var schemaTables = []string{"wallets", "wager_transactions"}

func tables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestMigrationsUpDownUp is the acceptance check of T3: every migration can
// be applied, fully reverted and applied again on a new database.
func TestMigrationsUpDownUp(t *testing.T) {
	t.Parallel()
	db := pgtest.NewEmpty(t)
	conn := db.Conn(t, pgtest.Owner)

	for round := 1; round <= 2; round++ {
		if err := pgtest.MigrateUp(ctx, conn); err != nil {
			t.Fatalf("up #%d: %v", round, err)
		}
		got := tables(t, conn)
		for _, want := range schemaTables {
			if !slices.Contains(got, want) {
				t.Fatalf("up #%d: table %s missing, have %v", round, want, got)
			}
		}
		if round == 1 {
			if err := pgtest.MigrateDown(ctx, conn); err != nil {
				t.Fatalf("down: %v", err)
			}
			if got := tables(t, conn); len(got) != 0 {
				t.Fatalf("down left tables behind: %v", got)
			}
		}
	}
}

// expectCode runs sql and requires it to fail with the given SQLSTATE.
func expectCode(t *testing.T, conn *pgx.Conn, code, sql string, args ...any) {
	t.Helper()
	_, err := conn.Exec(ctx, sql, args...)
	if got := pgtest.Code(err); got != code {
		t.Fatalf("%s\n  err = %v (SQLSTATE %q), want SQLSTATE %s", sql, err, got, code)
	}
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

const insertWallet = `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, now(), now())`

func newWallet(t *testing.T, conn *pgx.Conn, balance int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(t, conn, insertWallet, id, uuid.New(), "BRL", balance, 1)
	return id
}

func TestWalletConstraints(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)

	player := uuid.New()
	mustExec(t, app, insertWallet, uuid.New(), player, "BRL", 10000, 1)

	// Guarantee 8: non-negative balance enforced by the schema.
	expectCode(t, app, checkViolation, insertWallet, uuid.New(), uuid.New(), "BRL", -1, 1)
	expectCode(t, app, checkViolation, insertWallet, uuid.New(), uuid.New(), "BRL", 0, 0)
	expectCode(t, app, checkViolation, insertWallet, uuid.New(), uuid.New(), "brl", 0, 1)
	// One wallet per (player, currency).
	expectCode(t, app, uniqueViolation, insertWallet, uuid.New(), player, "BRL", 0, 1)
	mustExec(t, app, insertWallet, uuid.New(), player, "USD", 0, 1)
}

// TestWalletVersionTracksBalanceChanges: the database repeats the domain rule
// "version + 1 exactly when the balance changes", and freezes identity.
func TestWalletVersionTracksBalanceChanges(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	id := newWallet(t, app, 10000)

	mustExec(t, app, `UPDATE wallets SET balance_minor = 7500, version = 2, updated_at = now() WHERE id = $1`, id)
	expectCode(t, app, checkViolation, `UPDATE wallets SET balance_minor = 5000 WHERE id = $1`, id)
	expectCode(t, app, checkViolation, `UPDATE wallets SET version = 3 WHERE id = $1`, id)
	expectCode(t, app, checkViolation, `UPDATE wallets SET balance_minor = 5000, version = 4 WHERE id = $1`, id)
	expectCode(t, app, checkViolation, `UPDATE wallets SET player_id = $2 WHERE id = $1`, id, uuid.New())
	expectCode(t, app, checkViolation, `UPDATE wallets SET currency = 'USD' WHERE id = $1`, id)
	expectCode(t, app, checkViolation, `UPDATE wallets SET balance_minor = -1, version = 3 WHERE id = $1`, id)
}

func TestApplicationCannotDeleteWallets(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	id := newWallet(t, app, 0)
	expectCode(t, app, insufficientPrivilege, `DELETE FROM wallets WHERE id = $1`, id)
	expectCode(t, app, insufficientPrivilege, `TRUNCATE wallets`)
}

// row is a column -> value map for building test INSERTs, so each case states
// only what differs from a valid row.
type row map[string]any

func (r row) with(changes row) row {
	out := maps.Clone(r)
	for k, v := range changes {
		if v == nil {
			delete(out, k) // nil means "leave the column NULL"
		} else {
			out[k] = v
		}
	}
	return out
}

func insertRow(conn *pgx.Conn, table string, r row) error {
	cols := slices.Sorted(maps.Keys(r))
	ph := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = r[c]
	}
	_, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table,
		strings.Join(cols, ", "), strings.Join(ph, ", ")), args...)
	return err
}

func expectInsert(t *testing.T, conn *pgx.Conn, table, code string, r row) {
	t.Helper()
	err := insertRow(conn, table, r)
	if got := pgtest.Code(err); got != code {
		t.Fatalf("insert %v\n  err = %v (SQLSTATE %q), want %q", r, err, got, code)
	}
}

var payloadHash = sha256.Sum256([]byte("canonical payload"))

// externalBet is a valid, PENDING external BET for wallet.
func externalBet(wallet uuid.UUID, ext string) row {
	return row{
		"id": uuid.New(), "origin": "EXTERNAL", "kind": "BET", "status": "PENDING",
		"wallet_id": wallet, "player_id": uuid.New(), "currency": "BRL", "amount_minor": int64(2500),
		"provider_id": "provider-a", "external_transaction_id": ext, "idempotency_key": "key-" + ext,
		"payload_hash": payloadHash[:], "round_id": "round-1", "game_id": "game-1",
		"created_at": "2026-09-08T12:00:00Z", "updated_at": "2026-09-08T12:00:00Z",
	}
}

func opening(wallet uuid.UUID) row {
	return row{
		"id": uuid.New(), "origin": "INTERNAL", "kind": "OPENING", "status": "PROCESSED",
		"wallet_id": wallet, "player_id": uuid.New(), "currency": "BRL", "amount_minor": int64(10000),
		"result_balance_minor": int64(10000), "result_wallet_version": int64(1),
		"created_at": "2026-09-08T12:00:00Z", "updated_at": "2026-09-08T12:00:00Z",
		"processed_at": "2026-09-08T12:00:00Z",
	}
}

func processed(r row) row {
	return r.with(row{"status": "PROCESSED", "result_balance_minor": int64(7500),
		"result_wallet_version": int64(2), "processed_at": "2026-09-08T12:00:00Z"})
}

func TestWagerTransactionShape(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	w := newWallet(t, app, 10000)
	bet := externalBet(w, "tx-1")
	expectInsert(t, app, "wager_transactions", "", bet)

	for name, tt := range map[string]struct {
		r    row
		code string
	}{
		// Idempotency is scoped per provider (decision 3 of "Open questions").
		"same provider, same external id": {externalBet(w, "tx-1").with(row{"idempotency_key": "other"}), uniqueViolation},
		"same provider, same key":         {externalBet(w, "tx-2").with(row{"idempotency_key": "key-tx-1"}), uniqueViolation},
		"other provider, same ids":        {externalBet(w, "tx-1").with(row{"provider_id": "provider-b"}), ""},
		// §6.3: OPENING is internal and carries no provider metadata.
		"opening with provider": {opening(w).with(row{"provider_id": "provider-a"}), checkViolation},
		"external opening":      {externalBet(w, "tx-3").with(row{"kind": "OPENING"}), checkViolation},
		"external without key":  {externalBet(w, "tx-4").with(row{"idempotency_key": nil}), checkViolation},
		"external without hash": {externalBet(w, "tx-5").with(row{"payload_hash": nil}), checkViolation},
		"hash not sha-256":      {externalBet(w, "tx-6").with(row{"payload_hash": []byte("short")}), checkViolation},
		"external id too long":  {externalBet(w, strings.Repeat("x", 256)), checkViolation},
		// Zero policy (§7).
		"loss with amount":         {externalBet(w, "tx-7").with(row{"kind": "LOSS"}), checkViolation},
		"bet of zero":              {externalBet(w, "tx-8").with(row{"amount_minor": int64(0)}), checkViolation},
		"negative amount":          {externalBet(w, "tx-9").with(row{"amount_minor": int64(-1)}), checkViolation},
		"refund without reference": {externalBet(w, "tx-10").with(row{"kind": "REFUND"}), checkViolation},
		"bet with reference":       {externalBet(w, "tx-11").with(row{"reference_external_transaction_id": "x"}), checkViolation},
		// Status consistency.
		"rejected without code":              {externalBet(w, "tx-12").with(row{"status": "REJECTED"}), checkViolation},
		"processed without result":           {externalBet(w, "tx-13").with(row{"status": "PROCESSED"}), checkViolation},
		"code on a pending row":              {externalBet(w, "tx-14").with(row{"failure_code": "INSUFFICIENT_FUNDS"}), checkViolation},
		"pending reference without schedule": {externalBet(w, "tx-15").with(row{"status": "PENDING_REFERENCE"}), checkViolation},
		"unknown status":                     {externalBet(w, "tx-16").with(row{"status": "DONE"}), checkViolation},
		"unknown wallet":                     {externalBet(uuid.New(), "tx-17"), foreignKeyViolation},
	} {
		t.Run(name, func(t *testing.T) { expectInsert(t, app, "wager_transactions", tt.code, tt.r) })
	}
}

// TestSingleOpeningPerWallet: the schema prevents a duplicate opening credit.
func TestSingleOpeningPerWallet(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	w := newWallet(t, app, 10000)
	expectInsert(t, app, "wager_transactions", "", opening(w))
	expectInsert(t, app, "wager_transactions", uniqueViolation, opening(w))
}

// TestSingleSuccessfulReversal: a transaction is reversed successfully at most
// once, by REFUND or ROLLBACK (blueprint §6); rejected attempts do not count.
func TestSingleSuccessfulReversal(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	w := newWallet(t, app, 10000)
	bet := processed(externalBet(w, "bet-1"))
	expectInsert(t, app, "wager_transactions", "", bet)

	reversal := func(kind, ext string) row {
		return processed(externalBet(w, ext)).with(row{
			"kind": kind, "reference_external_transaction_id": "bet-1", "reference_transaction_id": bet["id"],
		})
	}
	expectInsert(t, app, "wager_transactions", "", reversal("REFUND", "refund-1"))
	rejected := reversal("ROLLBACK", "rollback-0").with(row{
		"status": "REJECTED", "failure_code": "ALREADY_REVERSED", "result_balance_minor": nil, "result_wallet_version": nil,
	})
	expectInsert(t, app, "wager_transactions", "", rejected)
	expectInsert(t, app, "wager_transactions", uniqueViolation, reversal("ROLLBACK", "rollback-1"))
	expectInsert(t, app, "wager_transactions", uniqueViolation, reversal("REFUND", "refund-2"))
}

// TestTerminalTransactionsAreFrozen: no transition out of PROCESSED,
// REJECTED or FAILED, and identity fields never change (defence in depth for
// the domain state machine).
func TestTerminalTransactionsAreFrozen(t *testing.T) {
	t.Parallel()
	db := pgtest.New(t)
	app := db.Conn(t, pgtest.App)
	w := newWallet(t, app, 10000)
	done := processed(externalBet(w, "done"))
	pending := externalBet(w, "pending")
	expectInsert(t, app, "wager_transactions", "", done)
	expectInsert(t, app, "wager_transactions", "", pending)

	expectCode(t, app, checkViolation, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'X' WHERE id = $1`, done["id"])
	expectCode(t, app, checkViolation, `UPDATE wager_transactions SET updated_at = now() WHERE id = $1`, done["id"])
	expectCode(t, app, checkViolation, `UPDATE wager_transactions SET amount_minor = 1 WHERE id = $1`, pending["id"])
	expectCode(t, app, checkViolation, `UPDATE wager_transactions SET payload_hash = $2 WHERE id = $1`, pending["id"], make([]byte, 32))
	expectCode(t, app, checkViolation, `UPDATE wager_transactions SET status = 'PENDING' WHERE id = $1`, pending["id"])
	mustExec(t, app, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'INSUFFICIENT_FUNDS',
		processed_at = now(), updated_at = now() WHERE id = $1`, pending["id"])
	expectCode(t, app, insufficientPrivilege, `DELETE FROM wager_transactions WHERE id = $1`, pending["id"])
}
