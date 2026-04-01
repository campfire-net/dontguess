package pricing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/pricing"
)

// TestInsurancePremium_LowRisk verifies the low-risk branch (latencyRatio <= 0.5)
// produces risk_multiplier = 1.05 and correct premium with no confidence penalty.
func TestInsurancePremium_LowRisk(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50, // above penalty threshold
	}
	// deadline = 10m → ratio = 2/10 = 0.2 → low risk, multiplier = 1.05
	result, err := pricing.InsurancePremium(entry, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ConfidencePenaltyApplied {
		t.Error("expected no confidence penalty for SampleCount=50")
	}
	// premium = 1000 * (1.05 - 1.0) / 1.0 ≈ 50 (ceiling rounding may give 50 or 51)
	if result.Premium < 50 || result.Premium > 51 {
		t.Errorf("expected premium in [50,51], got %d", result.Premium)
	}
	wantRatio := 2.0 / 10.0
	if diff := result.LatencyRatio - wantRatio; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected LatencyRatio=%.4f, got %.4f", wantRatio, result.LatencyRatio)
	}
}

// TestInsurancePremium_ModerateRisk verifies the moderate-risk branch (0.5 < ratio <= 0.8).
func TestInsurancePremium_ModerateRisk(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  6 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50,
	}
	// deadline = 10m → ratio = 6/10 = 0.6 → moderate, multiplier = 1.0 + 0.6*0.5 = 1.30
	result, err := pricing.InsurancePremium(entry, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMultiplier := 1.0 + 0.6*0.5
	if diff := result.RiskMultiplier - wantMultiplier; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected RiskMultiplier=%.4f, got %.4f", wantMultiplier, result.RiskMultiplier)
	}
	// premium = 1000 * (1.30 - 1.0) / 1.0 ≈ 300 (ceiling rounding may give 300 or 301)
	if result.Premium < 300 || result.Premium > 301 {
		t.Errorf("expected premium in [300,301], got %d", result.Premium)
	}
}

// TestInsurancePremium_HighRisk verifies the high-risk branch (0.8 < ratio <= 1.0).
func TestInsurancePremium_HighRisk(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  9 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50,
	}
	// deadline = 10m → ratio = 9/10 = 0.9
	// multiplier = 1.5 + (0.9 - 0.8) * 5.0 = 1.5 + 0.5 = 2.0
	result, err := pricing.InsurancePremium(entry, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMultiplier := 1.5 + (0.9-0.8)*5.0
	if diff := result.RiskMultiplier - wantMultiplier; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected RiskMultiplier=%.4f, got %.4f", wantMultiplier, result.RiskMultiplier)
	}
	// premium = 1000 * (2.0 - 1.0) / 1.0 = 1000
	if result.Premium != 1000 {
		t.Errorf("expected premium=1000, got %d", result.Premium)
	}
}

// TestInsurancePremium_Rejected verifies that a P90 exceeding the deadline
// returns ErrDeadlineUnguaranteed.
func TestInsurancePremium_Rejected(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  15 * time.Minute,
		FillRate:    0.9,
		SampleCount: 50,
	}
	// P90 > deadline → reject
	_, err := pricing.InsurancePremium(entry, 1000, 10*time.Minute)
	if !errors.Is(err, pricing.ErrDeadlineUnguaranteed) {
		t.Errorf("expected ErrDeadlineUnguaranteed, got %v", err)
	}
}

// TestInsurancePremium_LowFillRateInflatesPremium verifies that a low fill rate
// increases the premium (buyer pays more when workers frequently abandon tasks).
func TestInsurancePremium_LowFillRateInflatesPremium(t *testing.T) {
	t.Parallel()
	entryHighFill := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50,
	}
	entryLowFill := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    0.5,
		SampleCount: 50,
	}
	deadline := 10 * time.Minute
	basePrice := int64(1000)

	highFillResult, err := pricing.InsurancePremium(entryHighFill, basePrice, deadline)
	if err != nil {
		t.Fatalf("high fill: %v", err)
	}
	lowFillResult, err := pricing.InsurancePremium(entryLowFill, basePrice, deadline)
	if err != nil {
		t.Fatalf("low fill: %v", err)
	}
	if lowFillResult.Premium <= highFillResult.Premium {
		t.Errorf("expected low fill rate to increase premium: high=%d low=%d",
			highFillResult.Premium, lowFillResult.Premium)
	}
}

// TestInsurancePremium_ConfidencePenaltyApplied verifies that SampleCount < 30
// applies the 1.5x confidence penalty.
func TestInsurancePremium_ConfidencePenaltyApplied(t *testing.T) {
	t.Parallel()
	entryLowSamples := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    1.0,
		SampleCount: 10, // below threshold
	}
	entryHighSamples := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50, // above threshold
	}
	deadline := 10 * time.Minute
	basePrice := int64(1000)

	lowResult, err := pricing.InsurancePremium(entryLowSamples, basePrice, deadline)
	if err != nil {
		t.Fatalf("low samples: %v", err)
	}
	highResult, err := pricing.InsurancePremium(entryHighSamples, basePrice, deadline)
	if err != nil {
		t.Fatalf("high samples: %v", err)
	}
	if !lowResult.ConfidencePenaltyApplied {
		t.Error("expected ConfidencePenaltyApplied=true for SampleCount=10")
	}
	if highResult.ConfidencePenaltyApplied {
		t.Error("expected ConfidencePenaltyApplied=false for SampleCount=50")
	}
	// low premium should be 1.5x high premium (same base conditions)
	wantLowPremium := int64(float64(highResult.Premium) * 1.5)
	if lowResult.Premium < wantLowPremium-1 || lowResult.Premium > wantLowPremium+1 {
		t.Errorf("expected low premium≈%d (1.5x * %d), got %d",
			wantLowPremium, highResult.Premium, lowResult.Premium)
	}
}

// TestInsurancePremium_ZeroFillRateError verifies that a zero fill rate returns
// an error (cannot price a guarantee for a task that never completes).
func TestInsurancePremium_ZeroFillRateError(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    0.0,
		SampleCount: 50,
	}
	_, err := pricing.InsurancePremium(entry, 1000, 10*time.Minute)
	if err == nil {
		t.Error("expected error for zero fill rate, got nil")
	}
}

// TestInsurancePremium_ZeroDeadlineError verifies that a zero or negative deadline
// returns an error.
func TestInsurancePremium_ZeroDeadlineError(t *testing.T) {
	t.Parallel()
	entry := pricing.ActuarialEntry{
		P90Latency:  2 * time.Minute,
		FillRate:    1.0,
		SampleCount: 50,
	}
	_, err := pricing.InsurancePremium(entry, 1000, 0)
	if err == nil {
		t.Error("expected error for zero deadline, got nil")
	}
}
