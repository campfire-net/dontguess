package convention_test

import (
	"encoding/json"
	"testing"

	dgconv "github.com/campfire-net/dontguess/pkg/convention"
)

// buildDecl constructs a minimal convention declaration JSON payload for testing.
func buildDecl(convention, operation, version string, args []map[string]any) []byte {
	d := map[string]any{
		"convention":  convention,
		"version":     version,
		"operation":   operation,
		"description": "test declaration",
		"signing":     "member_key",
		"produces_tags": []map[string]any{
			{"tag": convention + ":" + operation, "cardinality": "exactly_one"},
		},
	}
	if args != nil {
		d["args"] = args
	}
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- Diff tests ----

func TestDiff_NoChange(t *testing.T) {
	args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	old := buildDecl("exchange", "put", "0.1", args)
	new := buildDecl("exchange", "put", "0.1", args)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindNone {
		t.Errorf("expected VersionKindNone, got %s", diff.Kind)
	}
	if len(diff.Breaking) != 0 {
		t.Errorf("expected no breaking changes, got %v", diff.Breaking)
	}
}

func TestDiff_AddOptionalArg_Minor(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "priority", "type": "integer"}, // optional
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "0.2", newArgs)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMinor {
		t.Errorf("expected VersionKindMinor, got %s", diff.Kind)
	}
	if len(diff.Breaking) != 0 {
		t.Errorf("expected no breaking changes, got %v", diff.Breaking)
	}
	if len(diff.Additions) != 1 || diff.Additions[0] != "priority" {
		t.Errorf("expected Additions=[priority], got %v", diff.Additions)
	}
}

func TestDiff_RemoveArg_Major(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "domain", "type": "string"},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		// domain removed
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "1.0", newArgs)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMajor {
		t.Errorf("expected VersionKindMajor, got %s", diff.Kind)
	}
	found := false
	for _, b := range diff.Breaking {
		if b.Kind == "arg_removed" && b.Field == "args.domain" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected arg_removed for domain, got %v", diff.Breaking)
	}
}

func TestDiff_AddRequiredArg_Major(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "seller_id", "type": "string", "required": true}, // new required arg
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "1.0", newArgs)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMajor {
		t.Errorf("expected VersionKindMajor, got %s", diff.Kind)
	}
	found := false
	for _, b := range diff.Breaking {
		if b.Kind == "arg_added_required" && b.Field == "args.seller_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected arg_added_required for seller_id, got %v", diff.Breaking)
	}
}

func TestDiff_RequiredToOptional_Major(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "token_cost", "type": "integer", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "token_cost", "type": "integer"}, // no longer required
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "1.0", newArgs)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMajor {
		t.Errorf("expected VersionKindMajor, got %s", diff.Kind)
	}
}

func TestDiff_OperationNameChanged_Major(t *testing.T) {
	args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", args)
	new := buildDecl("dontguess-exchange", "offer", "1.0", args) // renamed

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMajor {
		t.Errorf("expected VersionKindMajor, got %s", diff.Kind)
	}
	found := false
	for _, b := range diff.Breaking {
		if b.Kind == "operation_changed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected operation_changed breaking change, got %v", diff.Breaking)
	}
}

// ---- ValidateVersionBump tests ----

func TestValidateVersionBump_MinorChangeNeedsMajor(t *testing.T) {
	// Adding a required arg → breaking → must bump major.
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "seller_id", "type": "string", "required": true},
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "0.2", newArgs) // wrong: should be 1.0

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) == 0 {
		t.Error("expected validation error for breaking change without major bump")
	}
}

func TestValidateVersionBump_AddOptional_NeedsMinorBump(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "priority", "type": "integer"}, // optional
	}
	old := buildDecl("dontguess-exchange", "put", "0.1", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "0.2", newArgs) // correct minor bump

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) != 0 {
		t.Errorf("expected no errors for valid minor bump, got: %v", errs)
	}
}

func TestValidateVersionBump_AddOptional_WrongPatchBump(t *testing.T) {
	oldArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	newArgs := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "priority", "type": "integer"},
	}
	old := buildDecl("dontguess-exchange", "put", "0.1.0", oldArgs)
	new := buildDecl("dontguess-exchange", "put", "0.1.1", newArgs) // only patch bump

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) == 0 {
		t.Error("expected validation error for optional addition with only patch bump")
	}
}

func TestValidateVersionBump_VersionRegression(t *testing.T) {
	args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	old := buildDecl("dontguess-exchange", "put", "0.2", args)
	new := buildDecl("dontguess-exchange", "put", "0.1", args) // regression

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) == 0 {
		t.Error("expected validation error for version regression")
	}
}

// ---- Real declaration diff: put v0.1 -> v0.2 ----
//
// This mirrors the test scenario from the item spec: promoting put v0.1,
// then superseding with v0.2 (adding a new optional arg `priority`).

func TestDiff_PutV1ToV2_AddOptionalArg(t *testing.T) {
	// Minimal put v0.1 (core required fields only).
	putV1 := buildDecl("dontguess-exchange", "put", "0.1", []map[string]any{
		{"name": "description", "type": "string", "required": true, "max_length": 4096},
		{"name": "content_hash", "type": "string", "required": true, "max_length": 128},
		{"name": "token_cost", "type": "integer", "required": true},
		{"name": "content_size", "type": "integer", "required": true},
	})

	// put v0.2 adds optional `priority` arg.
	putV2 := buildDecl("dontguess-exchange", "put", "0.2", []map[string]any{
		{"name": "description", "type": "string", "required": true, "max_length": 4096},
		{"name": "content_hash", "type": "string", "required": true, "max_length": 128},
		{"name": "token_cost", "type": "integer", "required": true},
		{"name": "content_size", "type": "integer", "required": true},
		{"name": "priority", "type": "integer"}, // NEW optional arg
	})

	diff, err := dgconv.Diff(putV1, putV2)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff.Kind != dgconv.VersionKindMinor {
		t.Errorf("expected VersionKindMinor for new optional arg, got %s", diff.Kind)
	}
	if len(diff.Breaking) != 0 {
		t.Errorf("expected no breaking changes, got %v", diff.Breaking)
	}
	if len(diff.Additions) != 1 || diff.Additions[0] != "priority" {
		t.Errorf("expected Additions=[priority], got %v", diff.Additions)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) != 0 {
		t.Errorf("expected valid version bump, got: %v", errs)
	}
}

// ---- parseSemver overflow guard ----

// TestValidateVersionBump_OverflowInOldVersion exercises the parseSemver overflow
// guard via ValidateVersionBump: a version component that exceeds MaxInt must
// produce a validation error rather than silently wrapping.
func TestValidateVersionBump_OverflowInOldVersion(t *testing.T) {
	args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	// 99999999999999999999 has 20 digits — well past MaxInt64 (19 digits).
	overflowVersion := "99999999999999999999.0.0"
	old := buildDecl("dontguess-exchange", "put", overflowVersion, args)
	new := buildDecl("dontguess-exchange", "put", "1.0.0", args)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	// diff.OldVersion carries the raw string; ValidateVersionBump must reject it.
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) == 0 {
		t.Error("expected validation error for overflow version component, got none")
	}
}

// TestValidateVersionBump_OverflowInNewVersion exercises the overflow guard for
// the new version string.
func TestValidateVersionBump_OverflowInNewVersion(t *testing.T) {
	args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	overflowVersion := "0.99999999999999999999.0"
	old := buildDecl("dontguess-exchange", "put", "0.1.0", args)
	new := buildDecl("dontguess-exchange", "put", overflowVersion, args)

	diff, err := dgconv.Diff(old, new)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	errs := dgconv.ValidateVersionBump(diff)
	if len(errs) == 0 {
		t.Error("expected validation error for overflow version component, got none")
	}
}
