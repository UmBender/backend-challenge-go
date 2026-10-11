// Package wager models wager transactions: kinds, state machine, the rules
// that decide an operation, and the failure codes returned to providers.
package wager

import (
	"fmt"
)

// OPENING is internal; the other five kinds come from providers.
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	}
	return "", fmt.Errorf("wager: unknown kind %q", s)
}

// ParseExternalKind rejects OPENING with UNSUPPORTED_KIND, telling a
// forbidden kind apart from a typo.
func ParseExternalKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	case KindOpening:
		return "", invalid(CodeUnsupportedKind, "OPENING is reserved for internal wallet opening")
	}
	return "", invalid(CodeInvalidRequest, fmt.Sprintf("unknown kind %q", s))
}

func (k Kind) isReversal() bool { return k == KindRefund || k == KindRollback }

type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return st, nil
	}
	return "", fmt.Errorf("wager: unknown status %q", s)
}

func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// Origin lets the schema require provider metadata on external operations
// and forbid it on OPENING.
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)
