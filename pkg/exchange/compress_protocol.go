package exchange

import "fmt"

// compressionProtocol generates the full compression work order for a given
// content type. The returned string is self-contained: an agent with zero
// external knowledge can execute it. The protocol is Nyquist-sampled — preserve
// every signal that would change a downstream consumer's behavior, discard
// everything else.
//
// Acceptance criteria (enforced by the exchange on assign-complete):
//   - Size reduction ≥ 30% (size_compressed / size_original ≤ 0.70)
//   - Semantic similarity ≥ 0.85 (cosine similarity of embeddings)
//
// The content is base64-encoded in the entry. Decode it, compress per protocol,
// base64-encode the result, and submit via assign-complete.
func compressionProtocol(entryID, contentHash, contentType string, bounty int64) string {
	header := fmt.Sprintf(`COMPRESSION WORK ORDER
Entry: %s
Content hash: %s
Content type: %s
Bounty: %d scrip

RETRIEVAL
The entry content was delivered to you (hot/warm) or is retrievable via
the exchange campfire. Decode the base64 content field to get the raw text.

ACCEPTANCE CRITERIA (hard gates — both must pass)
  1. Size reduction ≥ 30%% (compressed size / original size ≤ 0.70)
  2. Semantic similarity ≥ 0.85 (embedding cosine distance)
  Rejection reasons: insufficient_reduction, low_similarity

SUBMISSION
Send assign-complete with:
  - evidence_hash: SHA-256 of compressed content (hex, "sha256:" prefix)
  - size_original: byte count of decoded original
  - size_compressed: byte count of compressed output
  - The compressed content itself (base64-encoded in payload)

`, entryID, contentHash, contentType, bounty)

	return header + compressionStrategy(contentType)
}

// compressionStrategy returns content-type-specific compression instructions.
// Each strategy follows the Nyquist principle: preserve every signal that would
// change a downstream consumer's task completion, discard everything else.
func compressionStrategy(contentType string) string {
	switch contentType {
	case "code":
		return `COMPRESSION STRATEGY: CODE
Preserve:
  - All function/method signatures (name, params, return types)
  - Core algorithm logic (the "what it does," not verbose implementations)
  - Error handling patterns (which errors are caught, what happens)
  - Public API surface (exported names, interfaces, types)
  - Import/dependency declarations
  - Comments that explain WHY (not WHAT — the code says what)
Discard:
  - Boilerplate (standard getter/setter bodies, trivial constructors)
  - Verbose variable declarations replaceable with type inference
  - Repeated patterns — show one instance, note "N similar"
  - Test scaffolding (setup/teardown) — keep assertions only
  - Commented-out code
Calibration: if a consumer using this to implement the same functionality
would write semantically equivalent code, the compression is correct.`

	case "analysis":
		return `COMPRESSION STRATEGY: ANALYSIS
Preserve:
  - Every conclusion and its supporting evidence chain
  - Quantitative findings (numbers, measurements, comparisons)
  - Methodology (how results were derived — not prose, just steps)
  - Constraints and assumptions that scope the analysis
  - Trade-offs identified and which side was chosen
  - Caveats and limitations
Discard:
  - Background/context that a domain practitioner already knows
  - Hedging language ("it should be noted that," "interestingly")
  - Restatements of the same finding in different words
  - Lengthy examples when one suffices
Calibration: if a consumer using this to make the same decision would
reach the same conclusion with the same confidence, the compression is correct.`

	case "summary":
		return `COMPRESSION STRATEGY: SUMMARY
Preserve:
  - Every distinct claim or finding (one line each)
  - Source references (what was summarized, where it came from)
  - Any numbers, dates, names, or specific facts
  - Scope boundaries (what is and isn't covered)
Discard:
  - Transitional prose ("In this section we will discuss...")
  - Restatements across sections
  - Meta-commentary about the summary itself
Calibration: if a consumer reading this compressed version would report
the same key takeaways as one who read the original, the compression is correct.`

	case "plan":
		return `COMPRESSION STRATEGY: PLAN
Preserve:
  - Every action item with its owner, deadline, and done condition
  - Dependencies between items (what blocks what)
  - Decisions already made (and alternatives killed, with reasons)
  - Open questions that block execution
  - Resource constraints (budget, time, personnel)
  - Success criteria for the overall plan
Discard:
  - Rationale for decisions that are already made (keep the decision, drop the debate)
  - Alternative approaches that were rejected (keep only the kill-reason)
  - Formatting and organizational scaffolding
Calibration: if a consumer executing this plan would take the same actions
in the same order with the same constraints, the compression is correct.`

	case "data":
		return `COMPRESSION STRATEGY: DATA
Preserve:
  - Schema/structure (column names, types, relationships)
  - Statistical summaries (min, max, mean, median, stddev, distribution shape)
  - Outliers and anomalies (specific values that deviate)
  - Key relationships between fields (correlations, dependencies)
  - Data quality notes (nulls, missing values, known issues)
  - Sample rows that illustrate the full range of values
Discard:
  - Repetitive rows that add no new information beyond the statistical summary
  - Formatting (alignment padding, decorative separators)
  - Metadata about the export process
Calibration: if a consumer using this to answer the same analytical questions
would reach the same answers (within statistical tolerance), the compression is correct.`

	case "review":
		return `COMPRESSION STRATEGY: REVIEW
Preserve:
  - Every finding (issue, suggestion, approval) with severity/priority
  - File/line references for code reviews
  - Blocking vs. non-blocking distinction
  - Specific fix suggestions (not vague "consider improving")
  - Approval/rejection verdict and conditions
Discard:
  - Praise without actionable content ("nice work on this part")
  - Restating what the code does (the code is available separately)
  - Style nits that are covered by automated linters
Calibration: if a consumer acting on this review would make the same
changes and reach the same approval state, the compression is correct.`

	default: // "other" or unknown
		return `COMPRESSION STRATEGY: GENERAL
Preserve:
  - Every distinct fact, claim, or instruction
  - Quantitative data (numbers, measurements, thresholds)
  - Logical structure (if/then relationships, sequences, dependencies)
  - Definitions of terms used later in the content
  - References to external resources
Discard:
  - Repeated information stated in multiple ways
  - Filler phrases and transitional prose
  - Examples beyond the first one that illustrates a point
  - Formatting-only content (decorative elements, excessive whitespace)
Calibration: if a consumer using this compressed version would complete
the same downstream task with the same outcome, the compression is correct.`
	}
}
