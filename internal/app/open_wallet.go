package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

type OpenWalletCommand struct {
	PlayerID       uuid.UUID
	InitialBalance money.Money
	// Defaults to the wallet id.
	CorrelationID string
}

// OpenWallet creates the wallet and, for a positive balance, its OPENING
// transaction, credit entry and events, in one commit.
type OpenWallet struct {
	tx    TxManager
	clock Clock
	ids   IDs
}

func NewOpenWallet(tx TxManager, clock Clock, ids IDs) *OpenWallet {
	return &OpenWallet{tx: tx, clock: clock, ids: ids}
}

// The database constraint, not a prior SELECT, decides uniqueness, so two
// concurrent openings cannot both succeed.
func (u *OpenWallet) Execute(ctx context.Context, cmd OpenWalletCommand) (*wallet.Wallet, error) {
	now := u.clock.Now()
	w, err := wallet.New(u.ids.New(), cmd.PlayerID, cmd.InitialBalance, now)
	if err != nil {
		return nil, err
	}
	correlation := cmd.CorrelationID
	if correlation == "" {
		correlation = w.ID().String()
	}
	// Domain objects are built before the transaction, keeping it short.
	var opening *wager.Opening
	if w.Balance().IsPositive() {
		o, err := wager.NewOpening(u.ids.New(), u.ids.New(), w, now)
		if err != nil {
			return nil, err
		}
		opening = &o
	}
	err = u.tx.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		if err := r.Wallets().Insert(ctx, w); err != nil {
			return err
		}
		if opening == nil {
			return nil // zero balance: no OPENING, no entry, no events (§9)
		}
		if err := r.Wagers().Insert(ctx, opening.Transaction); err != nil {
			return err
		}
		if err := r.Ledger().Append(ctx, opening.Entry); err != nil {
			return err
		}
		evs, err := envelopes(u.ids, correlation, "", now, opening.Events)
		if err != nil {
			return err
		}
		return r.Outbox().Append(ctx, evs...)
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}
