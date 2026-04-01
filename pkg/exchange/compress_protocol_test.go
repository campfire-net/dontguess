package exchange

import (
	"strings"
	"testing"
)

func TestCompressionProtocol_ContainsRequiredSections(t *testing.T) {
	t.Parallel()

	proto := compressionProtocol("entry-123", "sha256:abc", "code", 5000)

	required := []string{
		"COMPRESSION WORK ORDER",
		"Entry: entry-123",
		"Content hash: sha256:abc",
		"Content type: code",
		"Bounty: 5000 scrip",
		"RETRIEVAL",
		"ACCEPTANCE CRITERIA",
		"Size reduction",
		"Semantic similarity",
		"SUBMISSION",
		"assign-complete",
		"evidence_hash",
		"size_original",
		"size_compressed",
	}

	for _, s := range required {
		if !strings.Contains(proto, s) {
			t.Errorf("protocol missing required section: %q", s)
		}
	}
}

func TestCompressionProtocol_ContentTypeStrategies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		contentType string
		mustContain string // a phrase unique to this strategy
	}{
		{"code", "function/method signatures"},
		{"analysis", "conclusion"},
		{"summary", "distinct claim"},
		{"plan", "action item"},
		{"data", "Schema/structure"},
		{"review", "finding"},
		{"other", "GENERAL"},
		{"", "GENERAL"}, // unknown defaults to general
	}

	for _, tc := range cases {
		t.Run(tc.contentType, func(t *testing.T) {
			t.Parallel()
			proto := compressionProtocol("e-1", "sha256:x", tc.contentType, 100)
			if !strings.Contains(proto, tc.mustContain) {
				t.Errorf("content_type=%q: protocol missing strategy marker %q", tc.contentType, tc.mustContain)
			}
		})
	}
}

func TestCompressionProtocol_CalibrationRule(t *testing.T) {
	t.Parallel()

	// Every content type strategy must include a calibration rule.
	for _, ct := range []string{"code", "analysis", "summary", "plan", "data", "review", "other"} {
		proto := compressionProtocol("e-1", "sha256:x", ct, 100)
		if !strings.Contains(proto, "Calibration:") {
			t.Errorf("content_type=%q: missing calibration rule", ct)
		}
	}
}
