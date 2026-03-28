package convention_test

import (
	"testing"
	"time"

	dgconv "github.com/3dl-dev/dontguess/pkg/convention"
)

// ---- ValidateTags (denylist) -----------------------------------------------

func TestValidateTags_AllowedExactTags(t *testing.T) {
	allowed := []string{
		"exchange:buy",
		"exchange:match",
		"exchange:put",
		"exchange:settle",
		"dontguess:scrip-assign-pay",
		"dontguess:scrip-burn",
		"dontguess:scrip-buy-hold",
		"dontguess:scrip-dispute-refund",
		"dontguess:scrip-mint",
		"dontguess:scrip-put-pay",
		"dontguess:scrip-rate",
		"dontguess:scrip-settle",
	}
	errs := dgconv.ValidateTags(allowed)
	if len(errs) != 0 {
		t.Errorf("expected no errors for known exact tags, got: %v", errs)
	}
}

func TestValidateTags_AllowedWildcardTags(t *testing.T) {
	allowed := []string{
		"exchange:content-type:code",
		"exchange:content-type:analysis",
		"exchange:content-type:summary",
		"exchange:content-type:plan",
		"exchange:content-type:data",
		"exchange:content-type:review",
		"exchange:content-type:other",
		"exchange:domain:go",
		"exchange:domain:terraform",
		"exchange:phase:preview-request",
		"exchange:phase:buyer-accept",
		"exchange:phase:small-content-dispute",
		"exchange:verdict:accepted",
		"exchange:verdict:auto-refunded",
		"scrip:task:validate",
		"scrip:task:compress",
		"scrip:task:freshen",
		"scrip:reason:matching-fee",
		"scrip:reason:penalty",
		"scrip:amount:500",
		"scrip:to:abc123",
	}
	errs := dgconv.ValidateTags(allowed)
	if len(errs) != 0 {
		t.Errorf("expected no errors for known wildcard tags, got: %v", errs)
	}
}

func TestValidateTags_DeniedTags(t *testing.T) {
	denied := []string{
		"unknown:tag",
		"exchange:unknown-op",
		"scrip:unknown:value",
		"random",
		"exchange:content-type:unknown-value",
		"exchange:phase:not-a-phase",
		"exchange:verdict:not-a-verdict",
		"scrip:task:unknown-task",
		"scrip:reason:not-a-reason",
	}
	errs := dgconv.ValidateTags(denied)
	if len(errs) != len(denied) {
		t.Errorf("expected %d errors (one per denied tag), got %d: %v", len(denied), len(errs), errs)
	}
	for _, e := range errs {
		if e.Code != "tag_denied" {
			t.Errorf("expected code tag_denied, got %q in error: %v", e.Code, e)
		}
	}
}

func TestValidateTags_MixedSet(t *testing.T) {
	tags := []string{
		"exchange:put",          // allowed
		"malicious:inject",      // denied
		"exchange:domain:go",    // allowed
		"exchange:phase:wtf",    // denied (bad suffix for enum pattern)
	}
	errs := dgconv.ValidateTags(tags)
	if len(errs) != 2 {
		t.Errorf("expected 2 errors for 2 denied tags, got %d: %v", len(errs), errs)
	}
}

func TestValidateTags_EmptySlice(t *testing.T) {
	errs := dgconv.ValidateTags(nil)
	if len(errs) != 0 {
		t.Errorf("expected no errors for empty tag slice, got: %v", errs)
	}
}

// ---- ValidateTagPattern ---------------------------------------------------

func TestValidateTagPattern_ContentType_Valid(t *testing.T) {
	err := dgconv.ValidateTagPattern("exchange:content-type:code", "exchange:content-type:*")
	if err != nil {
		t.Errorf("expected no error for valid content-type tag, got: %v", err)
	}
}

func TestValidateTagPattern_ContentType_InvalidSuffix(t *testing.T) {
	err := dgconv.ValidateTagPattern("exchange:content-type:video", "exchange:content-type:*")
	if err == nil {
		t.Error("expected error for unknown content-type suffix, got nil")
	}
	if err.Code != "pattern_mismatch" {
		t.Errorf("expected code pattern_mismatch, got %q", err.Code)
	}
}

func TestValidateTagPattern_Phase_Valid(t *testing.T) {
	for _, phase := range dgconv.PhaseValues {
		err := dgconv.ValidateTagPattern(phase, "exchange:phase:*")
		if err != nil {
			t.Errorf("expected no error for phase %q, got: %v", phase, err)
		}
	}
}

func TestValidateTagPattern_Phase_InvalidSuffix(t *testing.T) {
	err := dgconv.ValidateTagPattern("exchange:phase:unknown-phase", "exchange:phase:*")
	if err == nil {
		t.Error("expected error for unknown phase suffix, got nil")
	}
}

func TestValidateTagPattern_Verdict_Valid(t *testing.T) {
	for _, v := range dgconv.VerdictValues {
		err := dgconv.ValidateTagPattern(v, "exchange:verdict:*")
		if err != nil {
			t.Errorf("expected no error for verdict %q, got: %v", v, err)
		}
	}
}

func TestValidateTagPattern_Domain_OpenWildcard(t *testing.T) {
	// exchange:domain:* is open — any suffix is valid.
	for _, domain := range []string{"exchange:domain:go", "exchange:domain:terraform", "exchange:domain:anything-goes"} {
		err := dgconv.ValidateTagPattern(domain, "exchange:domain:*")
		if err != nil {
			t.Errorf("expected no error for domain tag %q (open wildcard), got: %v", domain, err)
		}
	}
}

func TestValidateTagPattern_UnknownPatternKey(t *testing.T) {
	err := dgconv.ValidateTagPattern("foo:bar", "foo:*")
	if err == nil {
		t.Error("expected error for unknown pattern key, got nil")
	}
	if err.Code != "pattern_unknown" {
		t.Errorf("expected code pattern_unknown, got %q", err.Code)
	}
}

// ---- ValidateEnum ---------------------------------------------------------

func TestValidateEnum_ContentType_Valid(t *testing.T) {
	for _, v := range dgconv.ContentTypeValues {
		err := dgconv.ValidateContentType(v)
		if err != nil {
			t.Errorf("expected no error for valid content_type %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_ContentType_Invalid(t *testing.T) {
	err := dgconv.ValidateContentType("exchange:content-type:video")
	if err == nil {
		t.Error("expected error for invalid content_type, got nil")
	}
	if err.Code != "enum_mismatch" {
		t.Errorf("expected code enum_mismatch, got %q", err.Code)
	}
}

func TestValidateEnum_SettlePhase_Valid(t *testing.T) {
	for _, v := range dgconv.PhaseValues {
		err := dgconv.ValidateSettlePhase(v)
		if err != nil {
			t.Errorf("expected no error for valid phase %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_SettlePhase_Invalid(t *testing.T) {
	err := dgconv.ValidateSettlePhase("exchange:phase:finalize")
	if err == nil {
		t.Error("expected error for invalid phase, got nil")
	}
}

func TestValidateEnum_SettleVerdict_Valid(t *testing.T) {
	for _, v := range dgconv.VerdictValues {
		err := dgconv.ValidateSettleVerdict(v)
		if err != nil {
			t.Errorf("expected no error for valid verdict %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_DisputeType_Valid(t *testing.T) {
	for _, v := range dgconv.DisputeTypeValues {
		err := dgconv.ValidateDisputeType(v)
		if err != nil {
			t.Errorf("expected no error for valid dispute_type %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_DisputeType_Invalid(t *testing.T) {
	err := dgconv.ValidateDisputeType("wrong_reason")
	if err == nil {
		t.Error("expected error for invalid dispute_type, got nil")
	}
	if err.Code != "enum_mismatch" {
		t.Errorf("expected code enum_mismatch, got %q", err.Code)
	}
}

func TestValidateEnum_BurnReason_Valid(t *testing.T) {
	for _, v := range dgconv.BurnReasonValues {
		err := dgconv.ValidateBurnReason(v)
		if err != nil {
			t.Errorf("expected no error for valid burn reason %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_BurnReason_Invalid(t *testing.T) {
	err := dgconv.ValidateBurnReason("scrip:reason:unknown")
	if err == nil {
		t.Error("expected error for invalid burn reason, got nil")
	}
}

func TestValidateEnum_TaskType_Valid(t *testing.T) {
	for _, v := range dgconv.TaskTypeValues {
		err := dgconv.ValidateTaskType(v)
		if err != nil {
			t.Errorf("expected no error for valid task_type %q, got: %v", v, err)
		}
	}
}

func TestValidateEnum_TaskType_Invalid(t *testing.T) {
	err := dgconv.ValidateTaskType("scrip:task:unknown")
	if err == nil {
		t.Error("expected error for invalid task_type, got nil")
	}
}

func TestValidateEnum_EmptyValue(t *testing.T) {
	err := dgconv.ValidateContentType("")
	if err == nil {
		t.Error("expected error for empty content_type, got nil")
	}
}

// ---- ValidateContentHash --------------------------------------------------

func TestValidateContentHash_Valid(t *testing.T) {
	// Build a 64-char hex string
	hex64 := ""
	for i := 0; i < 64; i++ {
		hex64 += "a"
	}
	err := dgconv.ValidateContentHash("sha256:" + hex64)
	if err != nil {
		t.Errorf("expected no error for valid content_hash, got: %v", err)
	}
}

func TestValidateContentHash_ValidMixed(t *testing.T) {
	// 64 lowercase hex chars
	hash := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	err := dgconv.ValidateContentHash(hash)
	if err != nil {
		t.Errorf("expected no error for valid content_hash %q, got: %v", hash, err)
	}
}

func TestValidateContentHash_MissingPrefix(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	err := dgconv.ValidateContentHash(hash)
	if err == nil {
		t.Error("expected error for content_hash without sha256: prefix, got nil")
	}
	if err.Code != "pattern_mismatch" {
		t.Errorf("expected code pattern_mismatch, got %q", err.Code)
	}
}

func TestValidateContentHash_TooShort(t *testing.T) {
	err := dgconv.ValidateContentHash("sha256:abc123")
	if err == nil {
		t.Error("expected error for short content_hash, got nil")
	}
}

func TestValidateContentHash_UppercaseHex(t *testing.T) {
	// Pattern requires lowercase: [a-f0-9]
	hash := "sha256:0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
	err := dgconv.ValidateContentHash(hash)
	if err == nil {
		t.Error("expected error for uppercase hex in content_hash, got nil")
	}
}

func TestValidateContentHash_Empty(t *testing.T) {
	err := dgconv.ValidateContentHash("")
	if err == nil {
		t.Error("expected error for empty content_hash, got nil")
	}
}

// ---- CheckRateLimit -------------------------------------------------------

func TestCheckRateLimit_Buy_WithinLimit(t *testing.T) {
	result, err := dgconv.CheckRateLimit("buy", 30)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("expected allowed=true for count=30 at limit=30, got false: %s", result.Detail)
	}
	if result.Window != time.Minute {
		t.Errorf("expected window=1m for buy, got %v", result.Window)
	}
	if result.Limit != 30 {
		t.Errorf("expected limit=30 for buy, got %d", result.Limit)
	}
}

func TestCheckRateLimit_Buy_ExceedsLimit(t *testing.T) {
	result, err := dgconv.CheckRateLimit("buy", 31)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected allowed=false for count=31 at limit=30")
	}
	if result.Detail == "" {
		t.Error("expected non-empty Detail when limit exceeded")
	}
}

func TestCheckRateLimit_Put_WithinLimit(t *testing.T) {
	result, err := dgconv.CheckRateLimit("put", 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Errorf("expected allowed=true for count=50 at limit=50, got false: %s", result.Detail)
	}
	if result.Window != time.Hour {
		t.Errorf("expected window=1h for put, got %v", result.Window)
	}
}

func TestCheckRateLimit_Put_ExceedsLimit(t *testing.T) {
	result, err := dgconv.CheckRateLimit("put", 51)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected allowed=false for count=51 at limit=50")
	}
}

func TestCheckRateLimit_Match_Limits(t *testing.T) {
	r, err := dgconv.CheckRateLimit("match", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Allowed {
		t.Error("expected allowed=true for count=1")
	}
	if r.Limit != 30 {
		t.Errorf("expected limit=30 for match, got %d", r.Limit)
	}
	if r.Window != time.Minute {
		t.Errorf("expected window=1m for match, got %v", r.Window)
	}
}

func TestCheckRateLimit_Settle_Limits(t *testing.T) {
	r, err := dgconv.CheckRateLimit("settle", 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.Allowed {
		t.Error("expected allowed=true for count=50 at limit=50")
	}
	if r.Window != time.Hour {
		t.Errorf("expected window=1h for settle, got %v", r.Window)
	}
}

func TestCheckRateLimit_Zero_Messages_Allowed(t *testing.T) {
	result, err := dgconv.CheckRateLimit("buy", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed=true for count=0")
	}
}

func TestCheckRateLimit_UnknownOperation(t *testing.T) {
	_, err := dgconv.CheckRateLimit("scrip:mint", 1)
	if err == nil {
		t.Error("expected error for operation with no declared rate limit, got nil")
	}
}

func TestCheckRateLimit_UnknownOperationEmpty(t *testing.T) {
	_, err := dgconv.CheckRateLimit("", 0)
	if err == nil {
		t.Error("expected error for empty operation name, got nil")
	}
}
