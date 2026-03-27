// Package convention provides DontGuess-specific convention utilities: version
// comparison, breaking-change detection, and declaration diffing helpers used by
// the dontguess CLI's `convention supersede` command.
//
// It builds on top of github.com/campfire-net/campfire/pkg/convention, which
// owns parsing, lint, and registry publication.
package convention

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// VersionKind classifies the difference between two semver-compatible version strings.
type VersionKind string

const (
	// VersionKindNone means the two strings are identical.
	VersionKindNone VersionKind = "none"
	// VersionKindPatch is a patch-level change (x.y.Z).
	VersionKindPatch VersionKind = "patch"
	// VersionKindMinor is a minor-level change (x.Y.z).
	VersionKindMinor VersionKind = "minor"
	// VersionKindMajor is a major-level change (X.y.z) — always breaking.
	VersionKindMajor VersionKind = "major"
)

// BreakingChange describes a single incompatible difference between two
// convention declarations.
type BreakingChange struct {
	// Kind is a short tag identifying what changed (e.g. "arg_removed",
	// "arg_renamed", "convention_changed", "operation_changed").
	Kind string `json:"kind"`
	// Field is the dotted path within the declaration that changed.
	Field string `json:"field"`
	// Detail is a human-readable explanation.
	Detail string `json:"detail"`
}

// DiffResult is the output of Diff. It classifies the change and lists all
// detected breaking changes.
type DiffResult struct {
	// OldVersion is the version string from the old declaration.
	OldVersion string `json:"old_version"`
	// NewVersion is the version string from the new declaration.
	NewVersion string `json:"new_version"`
	// Kind classifies the version bump required.
	Kind VersionKind `json:"kind"`
	// Breaking contains all detected breaking changes. Empty when Kind != Major.
	Breaking []BreakingChange `json:"breaking,omitempty"`
	// Additions lists names of newly added args.
	Additions []string `json:"additions,omitempty"`
	// Deprecations lists names of args whose required flag changed true→false.
	Deprecations []string `json:"deprecations,omitempty"`
}

// Diff compares oldPayload and newPayload (raw JSON convention declarations) and
// returns a DiffResult describing the change classification. It does not perform
// lint — callers should lint both declarations before calling Diff.
//
// Breaking change policy (mirrors the item spec):
//   - Removed arg                           → major (breaking)
//   - Renamed arg (inferred by position)    → major (breaking)
//   - Required arg becoming optional        → major (breaking, existing callers may rely on it)
//   - Optional arg becoming required        → major (breaking)
//   - convention or operation name changed  → major (breaking)
//   - Added optional arg                    → minor
//   - Added required arg                    → major (breaking, missing in old callers)
//   - Changed description / rate_limit only → patch
func Diff(oldPayload, newPayload []byte) (*DiffResult, error) {
	var oldDecl, newDecl struct {
		Convention string `json:"convention"`
		Version    string `json:"version"`
		Operation  string `json:"operation"`
		Args       []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"args"`
	}

	if err := json.Unmarshal(oldPayload, &oldDecl); err != nil {
		return nil, fmt.Errorf("parsing old declaration: %w", err)
	}
	if err := json.Unmarshal(newPayload, &newDecl); err != nil {
		return nil, fmt.Errorf("parsing new declaration: %w", err)
	}

	result := &DiffResult{
		OldVersion: oldDecl.Version,
		NewVersion: newDecl.Version,
	}

	// Identity checks.
	if oldDecl.Convention != newDecl.Convention {
		result.Breaking = append(result.Breaking, BreakingChange{
			Kind:   "convention_changed",
			Field:  "convention",
			Detail: fmt.Sprintf("%q → %q", oldDecl.Convention, newDecl.Convention),
		})
	}
	if oldDecl.Operation != newDecl.Operation {
		result.Breaking = append(result.Breaking, BreakingChange{
			Kind:   "operation_changed",
			Field:  "operation",
			Detail: fmt.Sprintf("%q → %q", oldDecl.Operation, newDecl.Operation),
		})
	}

	// Arg diff.
	oldByName := make(map[string]struct{ required bool }, len(oldDecl.Args))
	for _, a := range oldDecl.Args {
		oldByName[a.Name] = struct{ required bool }{a.Required}
	}
	newByName := make(map[string]struct{ required bool }, len(newDecl.Args))
	for _, a := range newDecl.Args {
		newByName[a.Name] = struct{ required bool }{a.Required}
	}

	// Removed or required-changed args.
	for _, a := range oldDecl.Args {
		n, exists := newByName[a.Name]
		if !exists {
			result.Breaking = append(result.Breaking, BreakingChange{
				Kind:   "arg_removed",
				Field:  "args." + a.Name,
				Detail: fmt.Sprintf("arg %q was removed", a.Name),
			})
			continue
		}
		// Required flag changed.
		if a.Required && !n.required {
			// required→optional: breaking (callers may rely on presence).
			result.Breaking = append(result.Breaking, BreakingChange{
				Kind:   "arg_required_to_optional",
				Field:  "args." + a.Name,
				Detail: fmt.Sprintf("arg %q changed from required to optional", a.Name),
			})
		} else if !a.Required && n.required {
			// optional→required: breaking (old callers didn't need to provide it).
			result.Breaking = append(result.Breaking, BreakingChange{
				Kind:   "arg_optional_to_required",
				Field:  "args." + a.Name,
				Detail: fmt.Sprintf("arg %q changed from optional to required", a.Name),
			})
		}
	}

	// Added args.
	for _, a := range newDecl.Args {
		if _, exists := oldByName[a.Name]; !exists {
			if a.Required {
				result.Breaking = append(result.Breaking, BreakingChange{
					Kind:   "arg_added_required",
					Field:  "args." + a.Name,
					Detail: fmt.Sprintf("new required arg %q has no default — old callers will break", a.Name),
				})
			} else {
				result.Additions = append(result.Additions, a.Name)
			}
		}
	}

	// Deprecations: old required → new optional (already captured above as breaking;
	// also surface in Deprecations for documentation purposes).
	for name, old := range oldByName {
		if n, ok := newByName[name]; ok && old.required && !n.required {
			result.Deprecations = append(result.Deprecations, name)
		}
	}

	// Classify version kind.
	result.Kind = classifyVersionKind(result)
	return result, nil
}

// classifyVersionKind derives the required version bump from the DiffResult.
// It does NOT validate that the declared version strings actually reflect the
// required bump — that is the caller's job (ValidateVersionBump).
func classifyVersionKind(r *DiffResult) VersionKind {
	if len(r.Breaking) > 0 {
		return VersionKindMajor
	}
	if len(r.Additions) > 0 {
		return VersionKindMinor
	}
	if r.OldVersion != r.NewVersion {
		return VersionKindPatch
	}
	return VersionKindNone
}

// ValidateVersionBump checks that the version strings in oldPayload and newPayload
// are consistent with the diff result: major changes require a major bump, minor
// additions require at least a minor bump, etc.
//
// Returns a slice of validation errors. Empty slice means the bump is acceptable.
// This is intentionally lenient: a major bump for a minor change is allowed
// (operators may choose to be conservative).
func ValidateVersionBump(diff *DiffResult) []string {
	var errs []string

	oldMajor, oldMinor, oldPatch, err := parseSemver(diff.OldVersion)
	if err != nil {
		errs = append(errs, fmt.Sprintf("old version %q: %s", diff.OldVersion, err))
		return errs
	}
	newMajor, newMinor, newPatch, err := parseSemver(diff.NewVersion)
	if err != nil {
		errs = append(errs, fmt.Sprintf("new version %q: %s", diff.NewVersion, err))
		return errs
	}

	switch diff.Kind {
	case VersionKindMajor:
		// Must bump major. Any of these is acceptable:
		//   - newMajor > oldMajor
		if newMajor <= oldMajor {
			errs = append(errs, fmt.Sprintf(
				"breaking changes detected but version %s does not bump major (was %s) — use a major version bump",
				diff.NewVersion, diff.OldVersion,
			))
		}
	case VersionKindMinor:
		// Must bump at least minor (or major is fine too).
		if newMajor > oldMajor {
			break // major bump is fine
		}
		if newMajor < oldMajor {
			errs = append(errs, fmt.Sprintf("new version %s regresses major from %s", diff.NewVersion, diff.OldVersion))
			break
		}
		// Same major — check minor.
		if newMinor <= oldMinor {
			errs = append(errs, fmt.Sprintf(
				"new optional args added but version %s does not bump minor (was %s) — use at least a minor version bump",
				diff.NewVersion, diff.OldVersion,
			))
		}
		_ = newPatch
	case VersionKindPatch, VersionKindNone:
		// Any forward bump is acceptable; just reject regressions.
		if newMajor < oldMajor ||
			(newMajor == oldMajor && newMinor < oldMinor) ||
			(newMajor == oldMajor && newMinor == oldMinor && newPatch < oldPatch) {
			errs = append(errs, fmt.Sprintf("new version %s is not greater than old version %s", diff.NewVersion, diff.OldVersion))
		}
	}
	return errs
}

// parseSemver parses a "major.minor.patch" or "major.minor" or "major" version
// string. Missing components default to 0. Returns an error if the string is
// empty or contains non-numeric parts.
func parseSemver(v string) (major, minor, patch int, err error) {
	if v == "" {
		return 0, 0, 0, fmt.Errorf("empty version string")
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("too many version components in %q", v)
	}
	vals := [3]int{}
	for i, p := range parts {
		if p == "" {
			return 0, 0, 0, fmt.Errorf("empty component in version %q", v)
		}
		var n int
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, 0, 0, fmt.Errorf("non-numeric version component %q in %q", p, v)
			}
			digit := int(c - '0')
			if n > (math.MaxInt-digit)/10 {
				return 0, 0, 0, fmt.Errorf("version component %q overflows int in %q", p, v)
			}
			n = n*10 + digit
		}
		vals[i] = n
	}
	return vals[0], vals[1], vals[2], nil
}
