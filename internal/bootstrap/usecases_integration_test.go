//go:build integration

package bootstrap

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres/pgtest"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// TestUseCasesAreWired starts the real graph against a fresh migrated
// database and runs the use cases through what Fx built: the composition, not
// only each piece, is under test.
func TestUseCasesAreWired(t *testing.T) {
	requireEnv(t, "TEST_ADMIN_DATABASE_URL", "AWS_REGION", "AWS_ENDPOINT_URL")
	db := pgtest.New(t)
	t.Setenv("DATABASE_URL", db.URL(pgtest.App))
	t.Setenv("HTTP_ADDR", freeAddr(t))

	var (
		open   *app.OpenWallet
		submit *app.SubmitWager
		q      *app.Queries
		rec    *app.Reconcile
		pool   *pgxpool.Pool
	)
	fxApp := fxtest.New(t, Modules(), fx.NopLogger, fx.Populate(&open, &submit, &q, &rec, &pool))
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	ctx := context.Background()
	initial, _ := money.Parse("100.00", "BRL")
	w, err := open.Execute(ctx, app.OpenWalletCommand{PlayerID: uuid.New(), InitialBalance: initial})
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := money.Parse("30.00", "BRL")
	res, err := submit.Execute(ctx, app.SubmitCommand{IdempotencyKey: "k", Request: wager.Request{
		ProviderID: "provider-a", ExternalTransactionID: "tx-1", PlayerID: w.PlayerID(), WalletID: w.ID(),
		RoundID: "r", GameID: "g", Kind: wager.KindBet, Money: amount,
	}})
	if err != nil || res.Transaction.Status() != wager.StatusProcessed {
		t.Fatalf("submit: %v", err)
	}
	r, err := rec.Execute(ctx, w.ID())
	if err != nil || !r.Consistent {
		t.Fatalf("reconcile = %+v, %v", r, err)
	}

	// Every connection carries the lock and statement timeouts, so a stuck
	// lock becomes a fast, classified error instead of a hung request.
	for setting, want := range map[string]string{"lock_timeout": "5s", "statement_timeout": "15s"} {
		var got string
		if err := pool.QueryRow(ctx, "SHOW "+setting).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", setting, got, err, want)
		}
	}
}
