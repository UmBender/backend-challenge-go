//go:build integration

package postgres_test

import (
	"context"
	"slices"
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
var schemaTables = []string{"wallets"}

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
