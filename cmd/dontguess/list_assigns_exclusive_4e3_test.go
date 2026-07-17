package main

// Enforcement proof for dontguess-4e3 finding C (INDIVIDUAL tier) — the
// exclusive-sender filter in handleOpListAssigns (individual_ops.go): an assign
// whose ExclusiveSender is set to agent X must be EXCLUDED from agent Y's listing,
// INCLUDED in X's listing, and an OPEN assign (ExclusiveSender=="") must be
// included for everyone.
//
// Before this test, that filter branch (rec.ExclusiveSender != "" &&
// rec.ExclusiveSender != req.CallerKey) had NO coverage with a mismatching caller
// key. The test drives the REAL OpListAssigns over the REAL operator socket against
// a REAL serve engine whose state folded operator-authored assign records — no mock
// of the fold/filter under test.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// foldAssign folds an operator-authored exchange:assign record (task_type / reward /
// exclusive_sender) directly into the engine state via IngestLocalRecord, returning
// the assign ID. Individual-tier assigns are operator broadcasts, so the operator
// key is the authoring sender (applyAssign drops a non-operator author).
func foldAssign(t *testing.T, eng *exchange.Engine, operatorKey, taskType, exclusiveSender string, reward int64) string {
	t.Helper()
	assignID := randomLocalMsgID(t)
	payload, err := json.Marshal(map[string]any{
		"task_type":        taskType,
		"reward":           reward,
		"exclusive_sender": exclusiveSender,
	})
	if err != nil {
		t.Fatalf("marshal assign payload: %v", err)
	}
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:          assignID,
		CampfireID:  "local",
		Sender:      operatorKey,
		Payload:     payload,
		Tags:        []string{exchange.TagAssign},
		Antecedents: []string{},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(assign %s): %v", taskType, err)
	}
	return assignID
}

// listAssignsFor issues a real OpListAssigns over the socket for callerKey and
// returns the surfaced assigns.
func listAssignsFor(t *testing.T, sockPath, callerKey string) []assignsListEntry {
	t.Helper()
	var resp opListAssignsResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":         OpListAssigns,
		"caller_key": callerKey,
	}, &resp)
	if !resp.OK {
		t.Fatalf("OpListAssigns(caller=%s) failed: %s", callerKey, resp.Error)
	}
	return resp.Assigns
}

func hasEntryTaskType(assigns []assignsListEntry, taskType string) bool {
	for _, a := range assigns {
		if a.TaskType == taskType {
			return true
		}
	}
	return false
}

// TestOpListAssigns_ExclusiveSenderFilter_IndividualTier proves the exclusive-sender
// filter on the individual tier for BOTH a matching and a mismatching caller key,
// plus the open-to-everyone case.
func TestOpListAssigns_ExclusiveSenderFilter_IndividualTier(t *testing.T) {
	t.Parallel()

	eng, operatorKey := newIndividualTierEngineWithOperator(t)
	sockPath, _ := startSocketServer(t, eng)

	const (
		agentX = "aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000aaaa0000"
		agentY = "bbbb1111bbbb1111bbbb1111bbbb1111bbbb1111bbbb1111bbbb1111bbbb1111"
	)

	// One assign EXCLUSIVE to agent X, one OPEN to everyone.
	foldAssign(t, eng, operatorKey, "exclusive-task", agentX, 500)
	foldAssign(t, eng, operatorKey, "open-task", "", 300)

	// Agent X: sees BOTH the exclusive (targeted at X) and the open assign.
	xList := listAssignsFor(t, sockPath, agentX)
	if !hasEntryTaskType(xList, "exclusive-task") {
		t.Errorf("agent X listing MISSING its own exclusive assign; got %+v", xList)
	}
	if !hasEntryTaskType(xList, "open-task") {
		t.Errorf("agent X listing missing the open assign; got %+v", xList)
	}

	// Agent Y: the exclusive-to-X assign is EXCLUDED; the open assign is included.
	yList := listAssignsFor(t, sockPath, agentY)
	if hasEntryTaskType(yList, "exclusive-task") {
		t.Errorf("agent Y listing INCLUDED an assign exclusive to agent X (filter not enforced); got %+v", yList)
	}
	if !hasEntryTaskType(yList, "open-task") {
		t.Errorf("agent Y listing missing the open assign (open assigns must be visible to everyone); got %+v", yList)
	}

	// The exclusive assign X sees carries X's key as ExclusiveSender.
	var exclusiveForX *assignsListEntry
	for i := range xList {
		if xList[i].TaskType == "exclusive-task" {
			exclusiveForX = &xList[i]
		}
	}
	if exclusiveForX == nil {
		t.Fatalf("could not locate the exclusive assign in agent X's listing")
	}
	if exclusiveForX.ExclusiveSender != agentX {
		t.Errorf("exclusive assign ExclusiveSender = %q, want agent X's key %q", exclusiveForX.ExclusiveSender, agentX)
	}
}
