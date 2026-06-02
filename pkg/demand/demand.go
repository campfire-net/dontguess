// Package demand clusters buy-miss messages from the exchange into a
// stockable demand backlog. Each miss represents unmet demand: a buyer
// described a task, no cached inference matched, and the exchange posted
// a 70%-rate standing offer to anyone who computes and puts the result.
//
// # Clustering
//
// Misses are grouped by theme using keyword matching across seven pre-defined
// clusters derived from the §4 analysis in the measurement review doc:
//
//	campfire   (12 real misses)
//	audit      (9)
//	convention (8)
//	review     (6)
//	security   (FROST threshold + related)
//	test-gap   (untested endpoints, missing paths)
//	other      (uncategorized non-synthetic)
//
// # Synthetic exclusion
//
// Synthetic traffic is excluded from the backlog. A miss is synthetic when:
//   - its task starts with "regression-" or contains "regression-parallel-"
//   - its task contains "timeout-178"
//   - its task is exactly "test" or starts with "test " (case-insensitive)
//   - its task starts with "upgrade smoke test"
//
// These patterns match the load-test traffic described in the measurement
// review doc (§2, "Synthetic traffic pollutes metrics").
//
// # Assignable work queue
//
// Each non-synthetic miss surfaces as a BacklogItem: task text, cluster,
// offered_price_rate (always 70%), and the miss message ID (which can be
// referenced when fulfilling the standing offer via a put).
package demand

import (
	"encoding/json"
	"sort"
	"strings"
)

// BuyMissPayload is the JSON shape emitted by Engine.handleBuyMiss.
// Fields are defined in pkg/exchange/engine.go (handleBuyMiss).
type BuyMissPayload struct {
	Task             string `json:"task"`
	TaskHash         string `json:"task_hash"`
	OfferedPriceRate int    `json:"offered_price_rate"`
	ExpiresAt        string `json:"expires_at"`
	BuyMsgID         string `json:"buy_msg_id"`
}

// MissMessage is a minimal representation of an exchange:buy-miss message.
// Callers parse Messages and convert; this keeps the demand package decoupled
// from the exchange protocol layer.
type MissMessage struct {
	// ID is the exchange:buy-miss message ID (the standing offer message ID).
	ID string
	// Payload is the raw JSON payload of the exchange:buy-miss message.
	Payload []byte
	// Timestamp is the message timestamp (UnixNano). Used for recency ordering.
	Timestamp int64
}

// BacklogItem is one assignable work item in the demand backlog: a single
// non-synthetic miss with its cluster assignment.
type BacklogItem struct {
	// MissID is the ID of the exchange:buy-miss message (the standing offer).
	MissID string `json:"miss_id"`
	// BuyMsgID is the originating buy order ID (from the payload).
	BuyMsgID string `json:"buy_msg_id"`
	// Task is the buyer's task description — the work that needs to be done.
	Task string `json:"task"`
	// TaskHash is the SHA-256 hash of the task (from payload).
	TaskHash string `json:"task_hash"`
	// Cluster is the theme label (campfire, audit, convention, review,
	// security, test-gap, other).
	Cluster string `json:"cluster"`
	// OfferedPriceRate is the exchange's standing offer rate (70 = 70%
	// of token_cost). Put a matching result and earn this fraction of
	// your declared token_cost in scrip.
	OfferedPriceRate int `json:"offered_price_rate"`
	// ExpiresAt is the ISO-8601 expiry of the standing offer.
	ExpiresAt string `json:"expires_at"`
}

// Cluster is a named group of backlog items sharing a common theme.
type Cluster struct {
	// Name is the cluster label.
	Name string `json:"name"`
	// Count is the number of distinct misses in this cluster.
	Count int `json:"count"`
	// Items are the individual backlog items.
	Items []BacklogItem `json:"items"`
}

// Backlog is the full demand backlog produced from a set of miss messages.
type Backlog struct {
	// TotalMisses is the number of miss messages passed in (before filtering).
	TotalMisses int `json:"total_misses"`
	// SyntheticExcluded is the count of misses removed for being synthetic.
	SyntheticExcluded int `json:"synthetic_excluded"`
	// RealMisses is TotalMisses - SyntheticExcluded — the stockable backlog size.
	RealMisses int `json:"real_misses"`
	// Clusters are the themed groups, sorted by count descending.
	Clusters []Cluster `json:"clusters"`
}

// clusterRules maps cluster names to keywords. A miss is assigned to the first
// cluster whose keywords match the task text (case-insensitive substring match).
// Order determines priority for overlapping matches — more specific clusters
// appear earlier.
//
// Ordering rationale:
//  1. security/FROST: "frost", "cryptograph", "cold wallet" are unambiguous
//     signals. "auth gate" and "authentication gate" are checked before audit
//     (which also catches "endpoint").
//  2. review: "rpt review", "code review", "design review" — checked before
//     campfire to claim "RPT review of campfire SDK surface" as a review task.
//  3. convention: "revoke", "supersede" are unambiguous exchange convention
//     operations. "exchange convention" catches other convention tasks.
//  4. test-gap: "test gap", "test strategy" — checked before audit since both
//     deal with testing; the specific phrase is more selective.
//  5. audit: "audit", "missing error", "edge case", "error path" — broad
//     coverage-gap vocabulary, checked before campfire.
//  6. campfire: broadest named cluster; catches campfire SDK / protocol tasks
//     after all more specific clusters have claimed their tasks.
var clusterRules = []struct {
	name     string
	keywords []string
}{
	// security: FROST, cryptographic ceremonies, and auth gates
	{"security", []string{"frost", "cryptograph", "cold wallet", "signing ceremony", "auth gate", "authentication gate"}},
	// review: RPT, code review, design review — before campfire
	{"review", []string{"rpt review", "code review", "design review", "rpt "}},
	// convention: exchange convention operation lifecycle
	{"convention", []string{"revoke", "supersede", "exchange convention", "convention:put", "convention:buy", "convention:assign"}},
	// test-gap: test coverage gap analysis and test strategy
	{"test-gap", []string{"test gap", "test-gap", "test strategy", "test pattern"}},
	// audit: coverage audits, endpoint audits, missing error paths
	{"audit", []string{"audit", "missing error", "edge case gap", "edge case", "error path", "untested endpoint", "missing path"}},
	// campfire: campfire SDK, protocol, subscribe, and convention server
	{"campfire", []string{"campfire", "cf-protocol", "cf protocol", "campfire sdk", "convention server", "subscribe cursor", "convention declaration"}},
}

// IsSynthetic reports whether a task description represents synthetic load-test
// traffic that should be excluded from the real demand backlog.
//
// Exclusion rules (derived from measurement review §2 + live log inspection):
//   - starts with "regression-" or contains "regression-parallel-"
//   - contains "timeout-178"
//   - exactly equals "test" (bare load-test task word)
//   - starts with "upgrade smoke test" (the junk smoke-test put entry)
//   - matches "parallel-N-" pattern (load-test parallel buy series)
//   - starts with "validation-preflight-" (infra probes)
//   - starts with "e2e-startup-probe" or "test-investigation-probe" (probes)
//   - starts with "final-flake-attestation-" (CI probes)
//   - starts with "zzqq" (explicit nonsense task marker)
//   - is a bare short UUID or correlation ID (≤16 chars, no spaces)
//
// NOTE: Tasks like "test coverage audit", "test strategy", and "test gap" are
// NOT synthetic — they describe real engineering work and must remain in the
// backlog.
func IsSynthetic(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	if lower == "test" {
		return true
	}
	if strings.HasPrefix(lower, "regression-") {
		return true
	}
	if strings.Contains(lower, "regression-parallel-") {
		return true
	}
	if strings.Contains(lower, "timeout-178") {
		return true
	}
	if strings.HasPrefix(lower, "upgrade smoke test") {
		return true
	}
	// Load-test parallel buy series: "parallel-N-<runID>"
	if strings.HasPrefix(lower, "parallel-") {
		return true
	}
	// Infrastructure probes and health checks
	if strings.HasPrefix(lower, "validation-preflight-") {
		return true
	}
	if strings.HasPrefix(lower, "e2e-startup-probe") {
		return true
	}
	if strings.HasPrefix(lower, "test-investigation-probe") {
		return true
	}
	if strings.HasPrefix(lower, "final-flake-attestation-") {
		return true
	}
	// Explicit nonsense task marker ("zzqq nonsense xyzzy...")
	if strings.HasPrefix(lower, "zzqq") {
		return true
	}
	// Infra precondition check tasks
	if strings.HasPrefix(lower, "orchestrator precondition check:") {
		return true
	}
	// Post-identity-fix health checks and generic probe tasks
	if lower == "post-identity-fix health check" {
		return true
	}
	return false
}

// assignCluster returns the cluster name for the given task text by checking
// clusterRules in order. Returns "other" when no rule matches.
func assignCluster(task string) string {
	lower := strings.ToLower(task)
	for _, rule := range clusterRules {
		for _, kw := range rule.keywords {
			if strings.Contains(lower, kw) {
				return rule.name
			}
		}
	}
	return "other"
}

// BuildBacklog clusters a set of buy-miss messages into an assignable demand
// backlog. Synthetic misses are excluded. The returned Backlog has Clusters
// sorted by Count descending.
//
// msgs should be exchange:buy-miss messages (tagged TagBuyMiss + TagMatch).
// Each message's Payload must be a BuyMissPayload JSON object.
func BuildBacklog(msgs []MissMessage) Backlog {
	clusterMap := make(map[string][]BacklogItem)
	var synthetic int

	for _, m := range msgs {
		var p BuyMissPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			// Unparseable payload — treat as "other" with empty task.
			// This is defensive; the engine always writes valid JSON.
			p.BuyMsgID = m.ID
			p.OfferedPriceRate = 70
		}

		task := strings.TrimSpace(p.Task)

		if IsSynthetic(task) {
			synthetic++
			continue
		}

		cluster := assignCluster(task)

		item := BacklogItem{
			MissID:           m.ID,
			BuyMsgID:         p.BuyMsgID,
			Task:             task,
			TaskHash:         p.TaskHash,
			Cluster:          cluster,
			OfferedPriceRate: p.OfferedPriceRate,
			ExpiresAt:        p.ExpiresAt,
		}
		clusterMap[cluster] = append(clusterMap[cluster], item)
	}

	// Collect cluster names in a deterministic order.
	clusterNames := make([]string, 0, len(clusterMap))
	for name := range clusterMap {
		clusterNames = append(clusterNames, name)
	}
	// Sort by count descending, then name ascending for ties.
	sort.Slice(clusterNames, func(i, j int) bool {
		ci := len(clusterMap[clusterNames[i]])
		cj := len(clusterMap[clusterNames[j]])
		if ci != cj {
			return ci > cj
		}
		return clusterNames[i] < clusterNames[j]
	})

	clusters := make([]Cluster, 0, len(clusterNames))
	realMisses := 0
	for _, name := range clusterNames {
		items := clusterMap[name]
		realMisses += len(items)
		clusters = append(clusters, Cluster{
			Name:  name,
			Count: len(items),
			Items: items,
		})
	}

	return Backlog{
		TotalMisses:       len(msgs),
		SyntheticExcluded: synthetic,
		RealMisses:        realMisses,
		Clusters:          clusters,
	}
}
