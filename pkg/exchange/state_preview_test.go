package exchange

import (
	"testing"
)

// TestPreviewConstants verifies the new settle phase constants have correct values.
func TestPreviewConstants(t *testing.T) {
	if SettlePhaseStrPreviewRequest != "preview-request" {
		t.Errorf("SettlePhaseStrPreviewRequest = %q, want %q", SettlePhaseStrPreviewRequest, "preview-request")
	}
	if SettlePhaseStrPreview != "preview" {
		t.Errorf("SettlePhaseStrPreview = %q, want %q", SettlePhaseStrPreview, "preview")
	}
	if SettlePhaseStrSmallContentDispute != "small-content-dispute" {
		t.Errorf("SettlePhaseStrSmallContentDispute = %q, want %q", SettlePhaseStrSmallContentDispute, "small-content-dispute")
	}
	if SmallContentThreshold != 500 {
		t.Errorf("SmallContentThreshold = %d, want 500", SmallContentThreshold)
	}
	if SmallContentReputationPenalty != 3 {
		t.Errorf("SmallContentReputationPenalty = %d, want 3", SmallContentReputationPenalty)
	}
}

// TestNewStateInitializesPreviewMaps verifies that NewState initializes all new
// preview tracking maps as non-nil.
func TestNewStateInitializesPreviewMaps(t *testing.T) {
	s := NewState()

	if s.previewsByEntry == nil {
		t.Error("previewsByEntry is nil after NewState")
	}
	if s.previewCountByMatch == nil {
		t.Error("previewCountByMatch is nil after NewState")
	}
	if s.previewRequestToMatch == nil {
		t.Error("previewRequestToMatch is nil after NewState")
	}
	if s.previewToMatch == nil {
		t.Error("previewToMatch is nil after NewState")
	}
	if s.smallContentDisputes == nil {
		t.Error("smallContentDisputes is nil after NewState")
	}
}

// TestReplayResetsPreviewMaps verifies that Replay with an empty log leaves all
// new preview maps empty but non-nil.
func TestReplayResetsPreviewMaps(t *testing.T) {
	s := NewState()

	// Manually set values to confirm Replay resets them.
	s.previewsByEntry["entry1"] = map[string]string{"buyer1": "match1"}
	s.previewCountByMatch["match1"] = 2
	s.previewRequestToMatch["preq1"] = "match1"
	s.previewToMatch["prev1"] = "match1"
	s.smallContentDisputes["entry1"] = 5

	s.Replay(nil)

	if s.previewsByEntry == nil {
		t.Error("previewsByEntry is nil after Replay")
	}
	if len(s.previewsByEntry) != 0 {
		t.Errorf("previewsByEntry not empty after Replay: len=%d", len(s.previewsByEntry))
	}
	if s.previewCountByMatch == nil {
		t.Error("previewCountByMatch is nil after Replay")
	}
	if len(s.previewCountByMatch) != 0 {
		t.Errorf("previewCountByMatch not empty after Replay: len=%d", len(s.previewCountByMatch))
	}
	if s.previewRequestToMatch == nil {
		t.Error("previewRequestToMatch is nil after Replay")
	}
	if len(s.previewRequestToMatch) != 0 {
		t.Errorf("previewRequestToMatch not empty after Replay: len=%d", len(s.previewRequestToMatch))
	}
	if s.previewToMatch == nil {
		t.Error("previewToMatch is nil after Replay")
	}
	if len(s.previewToMatch) != 0 {
		t.Errorf("previewToMatch not empty after Replay: len=%d", len(s.previewToMatch))
	}
	if s.smallContentDisputes == nil {
		t.Error("smallContentDisputes is nil after Replay")
	}
	if len(s.smallContentDisputes) != 0 {
		t.Errorf("smallContentDisputes not empty after Replay: len=%d", len(s.smallContentDisputes))
	}
}
