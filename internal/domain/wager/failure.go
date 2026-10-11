package wager

// FailureCode values are part of the public contract: never rename one.
type FailureCode string

// Correctable input: never persisted, HTTP 400; the same key may be reused.
const (
	CodeInvalidRequest        FailureCode = "INVALID_REQUEST"
	CodeInvalidMoney          FailureCode = "INVALID_MONEY"
	CodeUnsupportedKind       FailureCode = "UNSUPPORTED_KIND"
	CodeMissingIdempotencyKey FailureCode = "MISSING_IDEMPOTENCY_KEY"
	CodeInvalidAmountForKind  FailureCode = "INVALID_AMOUNT_FOR_KIND"
)

// Business outcomes: persisted as REJECTED, replays return them (HTTP 422).
const (
	CodeInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	CodeReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeCurrencyMismatch          FailureCode = "CURRENCY_MISMATCH"
	CodeWalletPlayerMismatch      FailureCode = "WALLET_PLAYER_MISMATCH"
	CodeReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	CodeReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	CodeReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"
	CodeReferenceKindInvalid      FailureCode = "REFERENCE_KIND_INVALID"
	CodeReferenceAmountMismatch   FailureCode = "REFERENCE_AMOUNT_MISMATCH"
	CodeAlreadyReversed           FailureCode = "ALREADY_REVERSED"
)

// Permanent non-business failure (status FAILED), recorded only by
// asynchronous paths; the synchronous path answers 503 and commits nothing.
const CodeProcessingFailed FailureCode = "PROCESSING_FAILED"

// Nothing is persisted for an invalid request, so it is an error, not a Decision.
type InvalidRequestError struct {
	Code   FailureCode
	Reason string
}

func (e *InvalidRequestError) Error() string { return string(e.Code) + ": " + e.Reason }

func invalid(code FailureCode, reason string) error {
	return &InvalidRequestError{Code: code, Reason: reason}
}

// Classified with errors.As, never by string comparison.
type Rejection struct {
	Code FailureCode
}

func (r *Rejection) Error() string { return "wager rejected: " + string(r.Code) }
