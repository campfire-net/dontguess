// Package pricing — insurance premium formula.
//
// InsurancePremium computes the scrip cost to a buyer for an exchange-backed
// guarantee that a brokered-match assign will be delivered before a stated
// deadline. The formula is derived from the actuarial table stored in
// MarketParameters (updated by the slow loop every 4 hours).
//
// Formula (§3.2 of docs/design/semantic-matching-marketplace.md):
//
//	latency_ratio  = P90Latency / guarantee_deadline
//	risk_multiplier:
//	  <= 0.5           → 1.05 (low risk)
//	  (0.5, 0.8]       → 1.0 + ratio * 0.5 (moderate risk)
//	  (0.8, 1.0]       → 1.5 + (ratio - 0.8) * 5.0 (high risk, steep curve)
//	  > 1.0            → REJECT (P90 exceeds deadline)
//
//	premium = base_match_price * (risk_multiplier - 1.0) / fill_rate
//
// When SampleCount < 30, a 1.5x confidence_penalty multiplier is applied.
package pricing

import (
	"errors"
	"time"
)

// ErrDeadlineUnguaranteed is returned when the actuarial P90 latency exceeds
// the buyer's requested guarantee_deadline. The exchange will not issue a
// guarantee for this task type at this deadline.
var ErrDeadlineUnguaranteed = errors.New("pricing: P90 latency exceeds guarantee deadline — exchange cannot guarantee delivery")

// ConfidencePenaltyThreshold is the minimum number of observations before the
// confidence penalty is waived. Below this count, a 1.5x multiplier applies.
const ConfidencePenaltyThreshold = 30

// ConfidencePenaltyMultiplier is the multiplier applied to the premium when
// the actuarial table has fewer than ConfidencePenaltyThreshold samples.
const ConfidencePenaltyMultiplier = 1.5

// InsurancePremiumResult holds the computed premium and the intermediate values
// used in the calculation. Exported for diagnostics and tests.
type InsurancePremiumResult struct {
	// Premium is the scrip amount to charge the buyer.
	Premium int64
	// LatencyRatio is P90Latency / guarantee_deadline.
	LatencyRatio float64
	// RiskMultiplier is the computed risk multiplier (1.05–2.0 range).
	RiskMultiplier float64
	// ConfidencePenaltyApplied is true when SampleCount < ConfidencePenaltyThreshold.
	ConfidencePenaltyApplied bool
}

// InsurancePremium computes the insurance premium for a given task type,
// base match price, and buyer-requested guarantee deadline.
//
// entry is the ActuarialEntry for the task type (from MarketParameters.ActuarialTable).
// baseMatchPrice is the computed match price before the premium is added.
// guaranteeDeadline is how long the buyer is willing to wait.
//
// Returns ErrDeadlineUnguaranteed when P90Latency >= guaranteeDeadline.
// Returns an error when the fill_rate is zero (task type never completes).
func InsurancePremium(entry ActuarialEntry, baseMatchPrice int64, guaranteeDeadline time.Duration) (InsurancePremiumResult, error) {
	if guaranteeDeadline <= 0 {
		return InsurancePremiumResult{}, errors.New("pricing: guarantee_deadline must be positive")
	}
	if entry.FillRate <= 0 {
		return InsurancePremiumResult{}, errors.New("pricing: fill_rate is zero — task type never completes, cannot price guarantee")
	}

	latencyRatio := float64(entry.P90Latency) / float64(guaranteeDeadline)

	// Reject: P90 exceeds deadline — exchange cannot guarantee delivery.
	if latencyRatio > 1.0 {
		return InsurancePremiumResult{LatencyRatio: latencyRatio}, ErrDeadlineUnguaranteed
	}

	var riskMultiplier float64
	switch {
	case latencyRatio <= 0.5:
		riskMultiplier = 1.05
	case latencyRatio <= 0.8:
		riskMultiplier = 1.0 + latencyRatio*0.5
	default: // (0.8, 1.0]
		riskMultiplier = 1.5 + (latencyRatio-0.8)*5.0
	}

	// premium = base_match_price * (risk_multiplier - 1.0) / fill_rate
	rawPremium := float64(baseMatchPrice) * (riskMultiplier - 1.0) / entry.FillRate

	// Confidence penalty: low sample count → 1.5x multiplier.
	confidencePenaltyApplied := entry.SampleCount < ConfidencePenaltyThreshold
	if confidencePenaltyApplied {
		rawPremium *= ConfidencePenaltyMultiplier
	}

	// Round up to avoid under-charging (exchange absorbs the loss on misses).
	premium := int64(rawPremium)
	if float64(premium) < rawPremium {
		premium++
	}

	return InsurancePremiumResult{
		Premium:                  premium,
		LatencyRatio:             latencyRatio,
		RiskMultiplier:           riskMultiplier,
		ConfidencePenaltyApplied: confidencePenaltyApplied,
	}, nil
}
