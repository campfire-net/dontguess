// Package exchange implements the DontGuess exchange convention conformance checker.
//
// Provenance gating: exchange operations require different provenance levels
// from the Operator Provenance Convention v0.1. The mapping is defined by the
// exchange convention and the design rationale in dontguess-j9p:
//
//	anonymous (0): buy, read (inventory browse, price history)
//	claimed   (1): put, settle(buyer-*), settle(complete), settle(dispute)
//	contactable(2): assign operations
//	present   (3): mint, burn, rate-publish, settle(put-accept/reject), convention promote/supersede
//
// References:
//   - docs/convention/core-operations.md §9 (Conformance Checker)
//   - ~/projects/agentic-internet/docs/cf-brief.md §operator-provenance
//   - github.com/campfire-net/campfire/pkg/provenance
package exchange

import (
	"errors"
	"fmt"

	"github.com/campfire-net/campfire/pkg/provenance"
)

// Operation is an exchange operation type.
type Operation string

const (
	// Core operations (put, buy, match, settle).
	OperationPut    Operation = "put"
	OperationBuy    Operation = "buy"
	OperationMatch  Operation = "match"
	OperationSettle Operation = "settle"

	// Extended operations not in core convention v0.1, defined here for completeness.
	OperationAssign          Operation = "assign"
	OperationMint            Operation = "mint"
	OperationBurn            Operation = "burn"
	OperationRatePublish     Operation = "rate-publish"
	OperationConventionPromote   Operation = "convention-promote"
	OperationConventionSupersede Operation = "convention-supersede"

	// Read-only operations (inventory browse, price history).
	OperationInventoryRead    Operation = "inventory-read"
	OperationPriceHistoryRead Operation = "price-history-read"
)

// SettlePhase is a settlement phase within the settle operation.
// The provenance requirement depends on both the operation and the settle phase.
type SettlePhase string

const (
	SettlePhaseBuyerAccept SettlePhase = "buyer-accept"
	SettlePhaseBuyerReject SettlePhase = "buyer-reject"
	SettlePhasePutAccept   SettlePhase = "put-accept"
	SettlePhasePutReject   SettlePhase = "put-reject"
	SettlePhaseDeliver     SettlePhase = "deliver"
	SettlePhaseComplete    SettlePhase = "complete"
	SettlePhaseDispute     SettlePhase = "dispute"
)

// operationRequiredLevel maps each operation to its minimum provenance level.
// For settle, the phase determines the level — see settlePhaseRequiredLevel.
var operationRequiredLevel = map[Operation]provenance.Level{
	// anonymous (0): buy and read operations — no identity commitment required
	OperationBuy:              provenance.LevelAnonymous,
	OperationInventoryRead:    provenance.LevelAnonymous,
	OperationPriceHistoryRead: provenance.LevelAnonymous,

	// claimed (1): put and most settle phases — prevents throwaway-key spam
	OperationPut: provenance.LevelClaimed,

	// contactable (2): assign — exchange directs work to agents with reachable contact
	OperationAssign: provenance.LevelContactable,

	// present (3): privileged operations — mint, burn, rate-publish require
	// proven operator accountability (cf-brief §operator-provenance)
	OperationMint:                provenance.LevelPresent,
	OperationBurn:                provenance.LevelPresent,
	OperationRatePublish:         provenance.LevelPresent,
	OperationConventionPromote:   provenance.LevelPresent,
	OperationConventionSupersede: provenance.LevelPresent,

	// match is sent by the exchange operator (present required)
	OperationMatch: provenance.LevelPresent,
}

// settlePhaseRequiredLevel maps settle phases to provenance levels.
// Buyer-side phases require claimed; exchange-operator phases require present.
var settlePhaseRequiredLevel = map[SettlePhase]provenance.Level{
	// Buyer-side phases: claimed (seller has self-asserted identity)
	SettlePhaseBuyerAccept: provenance.LevelClaimed,
	SettlePhaseBuyerReject: provenance.LevelClaimed,
	SettlePhaseComplete:    provenance.LevelClaimed,
	SettlePhaseDispute:     provenance.LevelClaimed,

	// Exchange-operator phases: present (proven accountability)
	SettlePhasePutAccept: provenance.LevelPresent,
	SettlePhasePutReject: provenance.LevelPresent,
	SettlePhaseDeliver:   provenance.LevelPresent,
}

// ErrInsufficientProvenance is returned when a sender's provenance level does
// not meet the minimum required for the requested operation.
var ErrInsufficientProvenance = errors.New("exchange: insufficient provenance level for operation")

// ProvenanceChecker validates that a sender's provenance level satisfies the
// requirement for an exchange operation.
type ProvenanceChecker struct {
	store *provenance.Store
}

// ErrNilProvenanceStore is returned by NewProvenanceChecker when a nil store is provided.
var ErrNilProvenanceStore = errors.New("exchange: provenance store must not be nil")

// NewProvenanceChecker creates a ProvenanceChecker backed by the given store.
// Returns ErrNilProvenanceStore if store is nil.
func NewProvenanceChecker(store *provenance.Store) (*ProvenanceChecker, error) {
	if store == nil {
		return nil, ErrNilProvenanceStore
	}
	return &ProvenanceChecker{store: store}, nil
}

// Check returns nil if the sender's provenance level meets the requirement for
// the operation, or ErrInsufficientProvenance (wrapping details) if not.
//
// For OperationSettle, phase must be a non-empty SettlePhase. For all other
// operations, phase is ignored.
func (c *ProvenanceChecker) Check(senderKey string, op Operation, phase SettlePhase) error {
	required, err := RequiredLevel(op, phase)
	if err != nil {
		return err
	}

	actual := c.store.Level(senderKey)
	if actual < required {
		return fmt.Errorf("%w: operation=%q phase=%q requires %s, sender %q has %s",
			ErrInsufficientProvenance, op, phase, required, senderKey, actual)
	}
	return nil
}

// RequiredLevel returns the minimum provenance level required for an operation
// (and settle phase). Returns an error if the operation or phase is unknown.
func RequiredLevel(op Operation, phase SettlePhase) (provenance.Level, error) {
	if op == OperationSettle {
		if phase == "" {
			return 0, fmt.Errorf("exchange: settle operation requires a non-empty phase")
		}
		level, ok := settlePhaseRequiredLevel[phase]
		if !ok {
			return 0, fmt.Errorf("exchange: unknown settle phase %q", phase)
		}
		return level, nil
	}

	level, ok := operationRequiredLevel[op]
	if !ok {
		return 0, fmt.Errorf("exchange: unknown operation %q", op)
	}
	return level, nil
}
