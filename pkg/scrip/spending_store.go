// Package scrip implements the scrip ledger for the DontGuess exchange.
//
// Scrip is the exchange's internal currency, denominated in inference token cost.
// 1 scrip = the cost of 1 inference token at provider list price.
// Stored internally as micro-tokens (int64): 1 scrip = 1,000,000 micro-scrip.
//
// This package defines a SpendingStore interface that mirrors Forge's
// internal/ratelimit.SpendingStore. The interface is defined here rather than
// imported because Forge's ratelimit package is internal to that module.
// The semantics are identical: pre-decrement / adjust / refund pattern with
// ETag-based optimistic locking.
package scrip

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	// ErrBudgetExceeded is returned by DecrementBudget when balance < amount.
	// HTTP 402 equivalent.
	ErrBudgetExceeded = errors.New("scrip: budget exceeded")

	// ErrConflict is returned on ETag mismatch (concurrent update).
	// Caller should retry with a fresh GetBudget.
	ErrConflict = errors.New("scrip: conflict (etag mismatch)")

	// ErrReservationNotFound is returned when the reservation ID is unknown.
	ErrReservationNotFound = errors.New("scrip: reservation not found")
)

// Reservation tracks a pre-decremented scrip commitment for an in-flight
// exchange operation (buy-hold). Settled via Adjust or released via Refund.
type Reservation struct {
	// ID is the unique reservation identifier (hex-encoded random bytes).
	ID string
	// AgentKey is the Ed25519 pubkey (hex) of the agent whose balance was decremented.
	AgentKey string
	// RK is the row key used for the budget counter (always "scrip:balance").
	RK string
	// ETag is the etag at time of pre-decrement, used for re-check on adjust.
	ETag string
	// Amount is the amount held in micro-tokens.
	Amount int64
	// CreatedAt is when the reservation was made.
	CreatedAt time.Time
}

// SpendingStore is the storage backend for scrip balances and reservations.
//
// Budget counters are stored as int64 micro-tokens. ETags provide optimistic
// locking: read the ETag with GetBudget, pass it to DecrementBudget or AddBudget.
// On ETag mismatch (concurrent write), the operation returns ErrConflict and the
// caller retries.
//
// This interface mirrors Forge's internal/ratelimit.SpendingStore. If Forge ever
// exports that interface, this can be removed and the implementations adapted.
type SpendingStore interface {
	// DecrementBudget atomically subtracts amountMicro from the balance at (pk, rk)
	// provided the result would not go below zero.
	// Returns (newValue, newETag, nil) on success.
	// Returns ErrBudgetExceeded if value - amountMicro < 0.
	// Returns ErrConflict on ETag mismatch.
	DecrementBudget(ctx context.Context, pk, rk string, amountMicro int64, etag string) (newValue int64, newETag string, err error)

	// GetBudget reads the current balance and ETag at (pk, rk).
	// Returns (0, "", nil) if the counter does not exist.
	GetBudget(ctx context.Context, pk, rk string) (value int64, etag string, err error)

	// AddBudget adds amountMicro to the balance at (pk, rk).
	// If the counter does not exist, it is created with amountMicro as the initial value.
	// Returns (newValue, newETag, nil) on success.
	// Returns ErrConflict on ETag mismatch if etag is non-empty and stale.
	AddBudget(ctx context.Context, pk, rk string, amountMicro int64, etag string) (newValue int64, newETag string, err error)

	// SaveReservation persists a scrip reservation for an in-flight buy operation.
	SaveReservation(ctx context.Context, r Reservation) error

	// GetReservation retrieves a reservation by ID.
	// Returns ErrReservationNotFound if not present.
	GetReservation(ctx context.Context, id string) (Reservation, error)

	// DeleteReservation removes a reservation after it has been settled or refunded.
	// Returns ErrReservationNotFound if not present.
	DeleteReservation(ctx context.Context, id string) error
}

// BalanceKey is the row key used for all scrip balance counters.
const BalanceKey = "scrip:balance"
