package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

// TxManager owns transaction boundaries; repositories from Repos are bound to
// the current transaction (§4).
type TxManager interface {
	// READ COMMITTED; commits if fn returns nil. Errors come back classified.
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repos) error) error
	// REPEATABLE READ READ ONLY: one snapshot for every query.
	ReadSnapshot(ctx context.Context, fn func(ctx context.Context, r Repos) error) error
}

type Repos interface {
	Wallets() WalletRepo
	Wagers() WagerRepo
	Ledger() LedgerRepo
	Outbox() OutboxRepo
}

type WalletRepo interface {
	// ErrWalletAlreadyExists for a duplicate (player, currency).
	Insert(ctx context.Context, w *wallet.Wallet) error
	// Without locking; ErrWalletNotFound if absent.
	Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// SELECT ... FOR NO KEY UPDATE: the per-wallet serialization point.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// Only if the stored version is still expectedVersion.
	UpdateBalance(ctx context.Context, w *wallet.Wallet, expectedVersion int64) error
}

type WagerRepo interface {
	// For transactions that cannot conflict (OPENING).
	Insert(ctx context.Context, t *wager.Transaction) error
	// Returns false if (provider, key) or (provider, external id) exists; a
	// concurrent duplicate waits on the unique index. ErrWalletNotFound if the
	// wallet does not exist.
	InsertIfAbsent(ctx context.Context, t *wager.Transaction) (bool, error)
	// Writes the mutable part: status, outcome, schedule.
	Update(ctx context.Context, t *wager.Transaction) error
	// ErrTransactionNotFound if absent.
	Get(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
	// (nil, nil) when nothing matches.
	FindByKey(ctx context.Context, providerID, key string) (*wager.Transaction, error)
	FindByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error)
	// A PROCESSED REFUND or ROLLBACK already references id.
	HasProcessedReversal(ctx context.Context, id uuid.UUID) (bool, error)
	// ClaimDuePending locks one PENDING_REFERENCE row due at now, skipping
	// rows other workers hold; (nil, nil) when none is due.
	ClaimDuePending(ctx context.Context, now time.Time) (*wager.Transaction, error)
	// GetForUpdate reads and locks one transaction.
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
}

type LedgerRecord struct {
	Seq   int64
	Entry ledger.Entry
}

// Σ credits − Σ debits.
type LedgerTotals struct {
	NetMinor int64
	Count    int64
}

// No update or delete.
type LedgerRepo interface {
	Append(ctx context.Context, e ledger.Entry) error
	// Entries with seq > afterSeq, in seq order.
	Page(ctx context.Context, walletID uuid.UUID, afterSeq int64, limit int) ([]LedgerRecord, error)
	Totals(ctx context.Context, walletID uuid.UUID) (LedgerTotals, error)
}

type OutboxRepo interface {
	Append(ctx context.Context, events ...event.Envelope) error
}

type Clock interface{ Now() time.Time }

type IDs interface{ New() uuid.UUID }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Time-ordered UUIDs keep B-tree inserts append-mostly.
type UUIDv7 struct{}

// Falls back to v4 if the random source fails: unique, only unordered.
func (UUIDv7) New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
