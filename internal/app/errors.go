package app

import "errors"

// Adapters translate driver errors into these, and transports map them to
// responses.
var (
	// 404; SubmitWager persists nothing (the foreign key prevents it).
	ErrWalletNotFound = errors.New("wallet not found")
	// 409.
	ErrWalletAlreadyExists = errors.New("wallet already exists")
	// 404.
	ErrTransactionNotFound = errors.New("transaction not found")
	// 409 IDEMPOTENCY_KEY_REUSED: same (provider, key), different hash.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different payload")
	// 409 EXTERNAL_TRANSACTION_CONFLICT: same (provider, external id), another key.
	ErrExternalTransactionConflict = errors.New("external transaction already exists under another idempotency key")
	// A race detected by the database; the whole use case can be retried.
	ErrConcurrentConflict = errors.New("concurrent conflict")
	// Transient failure, nothing committed: 503 + Retry-After, SQS retries.
	ErrUnavailable = errors.New("dependency unavailable")
	// The database refused a write the domain should never produce: a bug, 500,
	// never retried.
	ErrInvariantViolation = errors.New("invariant violation")
)
