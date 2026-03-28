// Package convention provides DontGuess-specific convention utilities.
// This file adds programmatic enforcement for the dontguess-exchange convention:
// tag denylist, tag pattern safety, enum alignment, and rate limit constraints.
package convention

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ---- Tag vocabulary -------------------------------------------------------

// AllowedTagPrefixes is the complete set of tag prefixes that are valid on
// dontguess-exchange campfire messages.  Any tag not matching one of these
// prefixes (by exact match or wildcard prefix) is rejected by ValidateTags.
var AllowedTagPrefixes = []string{
	"exchange:",
	"scrip:",
	"dontguess:",
}

// allowedTagPatterns are the concrete patterns declared in produces_tags blocks
// across all exchange-core and exchange-scrip operations.  A tag is on the
// denylist if it does not match any of these patterns.
var allowedTagPatterns = []tagPattern{
	// exchange-core/buy.json
	{exact: "exchange:buy"},
	{prefix: "exchange:content-type:", allowedSuffixes: contentTypeSuffixes},
	{prefix: "exchange:domain:"},
	// exchange-core/match.json
	{exact: "exchange:match"},
	// exchange-core/put.json + put-v0.2.json
	{exact: "exchange:put"},
	// exchange-core/settle.json
	{exact: "exchange:settle"},
	{prefix: "exchange:phase:", allowedSuffixes: phaseSuffixes},
	{prefix: "exchange:verdict:", allowedSuffixes: verdictSuffixes},
	// exchange-scrip/assign-pay.json
	{exact: "dontguess:scrip-assign-pay"},
	{prefix: "scrip:task:", allowedSuffixes: taskSuffixes},
	// exchange-scrip/burn.json
	{exact: "dontguess:scrip-burn"},
	{prefix: "scrip:reason:", allowedSuffixes: reasonSuffixes},
	// exchange-scrip/buy-hold.json
	{exact: "dontguess:scrip-buy-hold"},
	// exchange-scrip/dispute-refund.json
	{exact: "dontguess:scrip-dispute-refund"},
	// exchange-scrip/mint.json
	{exact: "dontguess:scrip-mint"},
	{prefix: "scrip:amount:"},
	{prefix: "scrip:to:"},
	// exchange-scrip/put-pay.json
	{exact: "dontguess:scrip-put-pay"},
	// exchange-scrip/rate-publish.json
	{exact: "dontguess:scrip-rate"},
	// exchange-scrip/settle.json
	{exact: "dontguess:scrip-settle"},
}

// tagPattern describes one entry from a produces_tags declaration.
type tagPattern struct {
	// exact matches the tag literally when non-empty (and allowedSuffixes is nil).
	exact string
	// prefix matches tags that start with this string (wildcard pattern).
	prefix string
	// allowedSuffixes, if non-nil, restricts the wildcard to a finite enum of
	// values.  A tag with the right prefix but a suffix not in this slice fails.
	allowedSuffixes []string
}

func (p tagPattern) matches(tag string) bool {
	if p.exact != "" {
		return tag == p.exact
	}
	if !strings.HasPrefix(tag, p.prefix) {
		return false
	}
	if len(p.allowedSuffixes) == 0 {
		return true // open wildcard — any suffix is fine
	}
	for _, s := range p.allowedSuffixes {
		if tag == p.prefix+s {
			return true
		}
	}
	return false
}

// ---- Enum value sets -------------------------------------------------------

// contentTypeSuffixes are the bare suffix values for exchange:content-type:*.
var contentTypeSuffixes = []string{
	"code",
	"analysis",
	"summary",
	"plan",
	"data",
	"review",
	"other",
}

// ContentTypeValues are the full tag strings for content_type enum fields.
var ContentTypeValues = func() []string {
	v := make([]string, len(contentTypeSuffixes))
	for i, s := range contentTypeSuffixes {
		v[i] = "exchange:content-type:" + s
	}
	return v
}()

// phaseSuffixes are the bare suffix values for exchange:phase:*.
var phaseSuffixes = []string{
	"preview-request",
	"preview",
	"buyer-accept",
	"buyer-reject",
	"put-accept",
	"put-reject",
	"deliver",
	"complete",
	"dispute",
	"small-content-dispute",
}

// PhaseValues are the full tag strings for the settle.phase enum.
var PhaseValues = func() []string {
	v := make([]string, len(phaseSuffixes))
	for i, s := range phaseSuffixes {
		v[i] = "exchange:phase:" + s
	}
	return v
}()

// verdictSuffixes are the bare suffix values for exchange:verdict:*.
var verdictSuffixes = []string{
	"accepted",
	"rejected",
	"disputed",
	"auto-refunded",
}

// VerdictValues are the full tag strings for the settle.verdict enum.
var VerdictValues = func() []string {
	v := make([]string, len(verdictSuffixes))
	for i, s := range verdictSuffixes {
		v[i] = "exchange:verdict:" + s
	}
	return v
}()

// taskSuffixes are the bare suffix values for scrip:task:*.
var taskSuffixes = []string{
	"validate",
	"compress",
	"freshen",
}

// TaskTypeValues are the full tag strings for the assign-pay.task_type enum.
var TaskTypeValues = func() []string {
	v := make([]string, len(taskSuffixes))
	for i, s := range taskSuffixes {
		v[i] = "scrip:task:" + s
	}
	return v
}()

// reasonSuffixes are the bare suffix values for scrip:reason:*.
var reasonSuffixes = []string{
	"matching-fee",
	"operator-deflation",
	"penalty",
}

// BurnReasonValues are the full tag strings for the burn.reason enum.
var BurnReasonValues = func() []string {
	v := make([]string, len(reasonSuffixes))
	for i, s := range reasonSuffixes {
		v[i] = "scrip:reason:" + s
	}
	return v
}()

// DisputeTypeValues are the raw values for settle.dispute_type (not tag-based).
var DisputeTypeValues = []string{
	"content_mismatch",
	"quality_inadequate",
	"hash_invalid",
	"stale_content",
}

// ---- Rate limit definitions ------------------------------------------------

// RateLimit describes the declared per-sender rate limit for a campfire operation.
type RateLimit struct {
	// Operation is the operation name (e.g. "buy", "put", "match").
	Operation string
	// Max is the maximum number of messages allowed in the window.
	Max int
	// Window is the duration of the rate-limit window.
	Window time.Duration
}

// OperationRateLimits maps operation name to its declared RateLimit.
// Operations with no declared rate limit are absent from the map.
var OperationRateLimits = map[string]RateLimit{
	"buy":    {Operation: "buy", Max: 30, Window: time.Minute},
	"match":  {Operation: "match", Max: 30, Window: time.Minute},
	"put":    {Operation: "put", Max: 50, Window: time.Hour},
	"settle": {Operation: "settle", Max: 50, Window: time.Hour},
}

// ---- Pattern regexps -------------------------------------------------------

// ContentHashPattern is the declared pattern for content_hash fields:
//
//	^sha256:[a-f0-9]{64}$
var ContentHashPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ---- Validation functions --------------------------------------------------

// ValidationError is a single tagged validation failure.
type ValidationError struct {
	// Code is a short machine-readable identifier for the failure type.
	Code string
	// Field is the tag, field name, or operation that failed.
	Field string
	// Detail is a human-readable explanation.
	Detail string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Code, e.Field, e.Detail)
}

// ValidateTags checks that every tag in the provided slice is on the
// dontguess-exchange allowlist.  Tags not matching any declared pattern are
// returned as ValidationError values with Code="tag_denied".
func ValidateTags(tags []string) []ValidationError {
	var errs []ValidationError
	for _, tag := range tags {
		if !tagAllowed(tag) {
			errs = append(errs, ValidationError{
				Code:   "tag_denied",
				Field:  tag,
				Detail: fmt.Sprintf("tag %q is not declared in any dontguess-exchange produces_tags block", tag),
			})
		}
	}
	return errs
}

func tagAllowed(tag string) bool {
	for _, p := range allowedTagPatterns {
		if p.matches(tag) {
			return true
		}
	}
	return false
}

// ValidateTagPattern checks that a single tag matches the expected wildcard
// pattern (e.g. "exchange:content-type:*").  Returns a ValidationError with
// Code="pattern_mismatch" if it does not.
//
// patternKey is the human-friendly pattern name used in error messages (e.g.
// "exchange:content-type:*").
func ValidateTagPattern(tag, patternKey string) *ValidationError {
	for _, p := range allowedTagPatterns {
		// Find the pattern entry that owns this patternKey.
		candidate := p.prefix + "*"
		if candidate != patternKey && p.exact != patternKey {
			continue
		}
		if p.matches(tag) {
			return nil
		}
		// Pattern found but tag didn't match — report the failure.
		return &ValidationError{
			Code:  "pattern_mismatch",
			Field: tag,
			Detail: fmt.Sprintf("tag %q does not match pattern %q", tag, patternKey),
		}
	}
	// patternKey not in our registry — reject.
	return &ValidationError{
		Code:   "pattern_unknown",
		Field:  patternKey,
		Detail: fmt.Sprintf("pattern %q is not a known dontguess-exchange tag pattern", patternKey),
	}
}

// ValidateEnum checks that value is one of the allowedValues declared for an
// enum field.  Returns a ValidationError with Code="enum_mismatch" if not.
//
// fieldPath is used in the error message (e.g. "settle.phase").
func ValidateEnum(value, fieldPath string, allowedValues []string) *ValidationError {
	for _, v := range allowedValues {
		if value == v {
			return nil
		}
	}
	return &ValidationError{
		Code:  "enum_mismatch",
		Field: fieldPath,
		Detail: fmt.Sprintf(
			"%s value %q is not one of the declared enum values: [%s]",
			fieldPath, value, strings.Join(allowedValues, ", "),
		),
	}
}

// ValidateContentType validates a content_type field value against the declared
// enum for put / buy / settle.content_type.
func ValidateContentType(value string) *ValidationError {
	return ValidateEnum(value, "content_type", ContentTypeValues)
}

// ValidateSettlePhase validates a settle.phase field value against the declared
// enum.
func ValidateSettlePhase(value string) *ValidationError {
	return ValidateEnum(value, "settle.phase", PhaseValues)
}

// ValidateSettleVerdict validates a settle.verdict field value against the
// declared enum.
func ValidateSettleVerdict(value string) *ValidationError {
	return ValidateEnum(value, "settle.verdict", VerdictValues)
}

// ValidateDisputeType validates a settle.dispute_type field value.
func ValidateDisputeType(value string) *ValidationError {
	return ValidateEnum(value, "settle.dispute_type", DisputeTypeValues)
}

// ValidateBurnReason validates a scrip:burn.reason field value.
func ValidateBurnReason(value string) *ValidationError {
	return ValidateEnum(value, "scrip:burn.reason", BurnReasonValues)
}

// ValidateTaskType validates a scrip:assign-pay.task_type field value.
func ValidateTaskType(value string) *ValidationError {
	return ValidateEnum(value, "scrip:assign-pay.task_type", TaskTypeValues)
}

// ValidateContentHash validates a content_hash field against the declared
// pattern ^sha256:[a-f0-9]{64}$.
func ValidateContentHash(value string) *ValidationError {
	if ContentHashPattern.MatchString(value) {
		return nil
	}
	return &ValidationError{
		Code:  "pattern_mismatch",
		Field: "content_hash",
		Detail: fmt.Sprintf(
			"content_hash %q does not match required pattern ^sha256:[a-f0-9]{64}$",
			value,
		),
	}
}

// ---- Rate limit helpers ---------------------------------------------------

// RateLimitResult is the output of CheckRateLimit.
type RateLimitResult struct {
	// Allowed is true when the message count is within the declared limit.
	Allowed bool
	// Operation is the operation that was checked.
	Operation string
	// Limit is the declared maximum.
	Limit int
	// Window is the rate-limit window duration.
	Window time.Duration
	// Count is the number of messages observed.
	Count int
	// Detail is a human-readable explanation when Allowed is false.
	Detail string
}

// CheckRateLimit evaluates whether count messages from a single sender in the
// given window would exceed the declared rate limit for operation.
//
// Returns an error if operation has no declared rate limit.  Returns
// RateLimitResult.Allowed=false when count > rl.Max.
func CheckRateLimit(operation string, count int) (RateLimitResult, error) {
	rl, ok := OperationRateLimits[operation]
	if !ok {
		return RateLimitResult{}, fmt.Errorf("no rate limit declared for operation %q", operation)
	}
	allowed := count <= rl.Max
	result := RateLimitResult{
		Allowed:   allowed,
		Operation: operation,
		Limit:     rl.Max,
		Window:    rl.Window,
		Count:     count,
	}
	if !allowed {
		result.Detail = fmt.Sprintf(
			"operation %q: %d messages exceeds limit of %d per sender per %s",
			operation, count, rl.Max, rl.Window,
		)
	}
	return result, nil
}
