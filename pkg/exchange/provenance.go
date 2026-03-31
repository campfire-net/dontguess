// Package exchange implements the DontGuess exchange convention conformance checker.
//
// Provenance gating: exchange operations require different provenance levels
// from the Operator Provenance Convention v0.1. Default mapping:
//
//	anonymous (0): buy, read (inventory browse, price history)
//	claimed   (1): put, settle(buyer-*), settle(complete), settle(dispute)
//	contactable(2): assign operations
//	present   (3): mint, burn, rate-publish, settle(put-accept/reject), convention promote/supersede
//
// Operators can override these defaults in the exchange config file
// (provenance_levels section) without rebuilding.
//
// References:
//   - docs/convention/core-operations.md §9 (Conformance Checker)
//   - ~/projects/agentic-internet/docs/cf-brief.md §operator-provenance
//   - github.com/campfire-net/campfire/pkg/provenance
package exchange

import (
	"errors"
	"fmt"
	"time"

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
	OperationAssign              Operation = "assign"
	OperationMint                Operation = "mint"
	OperationBurn                Operation = "burn"
	OperationRatePublish         Operation = "rate-publish"
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

// defaultOperationLevels is the compiled-in default mapping.
var defaultOperationLevels = map[Operation]provenance.Level{
	OperationBuy:              provenance.LevelAnonymous,
	OperationInventoryRead:    provenance.LevelAnonymous,
	OperationPriceHistoryRead: provenance.LevelAnonymous,
	OperationPut:              provenance.LevelClaimed,
	OperationAssign:           provenance.LevelContactable,
	OperationMint:             provenance.LevelPresent,
	OperationBurn:             provenance.LevelPresent,
	OperationRatePublish:      provenance.LevelPresent,
	OperationConventionPromote:   provenance.LevelPresent,
	OperationConventionSupersede: provenance.LevelPresent,
	OperationMatch:            provenance.LevelPresent,
}

// defaultSettlePhaseLevels is the compiled-in default for settle phases.
var defaultSettlePhaseLevels = map[SettlePhase]provenance.Level{
	SettlePhaseBuyerAccept: provenance.LevelClaimed,
	SettlePhaseBuyerReject: provenance.LevelClaimed,
	SettlePhaseComplete:    provenance.LevelClaimed,
	SettlePhaseDispute:     provenance.LevelClaimed,
	SettlePhasePutAccept:   provenance.LevelPresent,
	SettlePhasePutReject:   provenance.LevelPresent,
	SettlePhaseDeliver:     provenance.LevelPresent,
}

// ProvenanceLevels configures the minimum provenance level required for each
// exchange operation. Stored in the exchange config JSON as provenance_levels.
//
// Keys are operation names (e.g. "put", "buy", "match") or "settle:<phase>"
// for settle phases (e.g. "settle:put-accept", "settle:buyer-reject").
// Values are level names: "anonymous", "claimed", "contactable", "present".
//
// Only overridden keys need to be present — missing keys use compiled defaults.
type ProvenanceLevels map[string]string

// levelNames maps level name strings to provenance.Level values.
var levelNames = map[string]provenance.Level{
	"anonymous":   provenance.LevelAnonymous,
	"claimed":     provenance.LevelClaimed,
	"contactable": provenance.LevelContactable,
	"present":     provenance.LevelPresent,
}

// ErrInsufficientProvenance is returned when a sender's provenance level does
// not meet the minimum required for the requested operation.
var ErrInsufficientProvenance = errors.New("exchange: insufficient provenance level for operation")

// ProvenanceChecker validates that a sender's provenance level satisfies the
// requirement for an exchange operation.
type ProvenanceChecker struct {
	store        *provenance.Store
	opLevels     map[Operation]provenance.Level
	settleLevels map[SettlePhase]provenance.Level
}

// ErrNilProvenanceStore is returned by NewProvenanceChecker when a nil store is provided.
var ErrNilProvenanceStore = errors.New("exchange: provenance store must not be nil")

// NewProvenanceChecker creates a ProvenanceChecker backed by the given store.
// If overrides is non-nil, entries override the compiled defaults.
// Returns ErrNilProvenanceStore if store is nil.
func NewProvenanceChecker(store *provenance.Store, overrides ...ProvenanceLevels) (*ProvenanceChecker, error) {
	if store == nil {
		return nil, ErrNilProvenanceStore
	}

	// Start with copies of the defaults.
	opLevels := make(map[Operation]provenance.Level, len(defaultOperationLevels))
	for k, v := range defaultOperationLevels {
		opLevels[k] = v
	}
	settleLevels := make(map[SettlePhase]provenance.Level, len(defaultSettlePhaseLevels))
	for k, v := range defaultSettlePhaseLevels {
		settleLevels[k] = v
	}

	// Apply overrides from config.
	if len(overrides) > 0 && overrides[0] != nil {
		for key, levelName := range overrides[0] {
			level, ok := levelNames[levelName]
			if !ok {
				return nil, fmt.Errorf("exchange: unknown provenance level %q for key %q", levelName, key)
			}
			// "settle:<phase>" keys go to settleLevels; everything else to opLevels.
			if len(key) > 7 && key[:7] == "settle:" {
				settleLevels[SettlePhase(key[7:])] = level
			} else {
				opLevels[Operation(key)] = level
			}
		}
	}

	return &ProvenanceChecker{
		store:        store,
		opLevels:     opLevels,
		settleLevels: settleLevels,
	}, nil
}

// Check returns nil if the sender's provenance level meets the requirement for
// the operation, or ErrInsufficientProvenance (wrapping details) if not.
func (c *ProvenanceChecker) Check(senderKey string, op Operation, phase SettlePhase) error {
	return c.CheckAt(senderKey, op, phase, time.Now())
}

// CheckAt is identical to Check but evaluates provenance freshness at the given
// time t instead of time.Now().
func (c *ProvenanceChecker) CheckAt(senderKey string, op Operation, phase SettlePhase, t time.Time) error {
	required, err := c.RequiredLevel(op, phase)
	if err != nil {
		return err
	}

	actual := c.store.LevelAt(senderKey, t)
	if actual < required {
		return fmt.Errorf("%w: operation=%q phase=%q requires %s, sender %q has %s",
			ErrInsufficientProvenance, op, phase, required, senderKey, actual)
	}
	return nil
}

// RequiredLevel returns the minimum provenance level required for an operation
// (and settle phase). Returns an error if the operation or phase is unknown.
func (c *ProvenanceChecker) RequiredLevel(op Operation, phase SettlePhase) (provenance.Level, error) {
	if op == OperationSettle {
		if phase == "" {
			return 0, fmt.Errorf("exchange: settle operation requires a non-empty phase")
		}
		level, ok := c.settleLevels[phase]
		if !ok {
			return 0, fmt.Errorf("exchange: unknown settle phase %q", phase)
		}
		return level, nil
	}

	level, ok := c.opLevels[op]
	if !ok {
		return 0, fmt.Errorf("exchange: unknown operation %q", op)
	}
	return level, nil
}

// RequiredLevel is a package-level convenience that uses the compiled defaults.
// Prefer the method on ProvenanceChecker for production use.
func RequiredLevel(op Operation, phase SettlePhase) (provenance.Level, error) {
	if op == OperationSettle {
		if phase == "" {
			return 0, fmt.Errorf("exchange: settle operation requires a non-empty phase")
		}
		level, ok := defaultSettlePhaseLevels[phase]
		if !ok {
			return 0, fmt.Errorf("exchange: unknown settle phase %q", phase)
		}
		return level, nil
	}

	level, ok := defaultOperationLevels[op]
	if !ok {
		return 0, fmt.Errorf("exchange: unknown operation %q", op)
	}
	return level, nil
}
