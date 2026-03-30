package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cfconvention "github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"
)

// ---- helpers ----

// testBuildDecl creates a minimal convention declaration JSON payload.
func testBuildDecl(conv, operation, version string, args []map[string]any) []byte {
	d := map[string]any{
		"convention":  conv,
		"version":     version,
		"operation":   operation,
		"description": "test declaration",
		"signing":     "member_key",
		"produces_tags": []map[string]any{
			{"tag": conv + ":" + operation, "cardinality": "exactly_one"},
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

// testWriteDeclToStore writes a convention declaration to the campfire via client.Send.
// The client must already have membership recorded for campfireID.
// Used by tests that exercise the full protocol stack (testSetupCampfire tests).
func testWriteDeclToStore(t *testing.T, client *protocol.Client, campfireID string, payload []byte, supersedes string) string {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if supersedes != "" {
		b, _ := json.Marshal(supersedes)
		raw["supersedes"] = b
	}
	finalPayload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, err := client.Send(protocol.SendRequest{
		CampfireID: campfireID,
		Payload:    finalPayload,
		Tags:       []string{cfconvention.ConventionOperationTag},
	})
	if err != nil {
		t.Fatalf("client.Send: %v", err)
	}
	return msg.ID
}

// testAddDeclToStore writes a convention declaration directly to the store,
// bypassing the transport. Use for early-exit tests where no campfire transport
// is set up and send never reaches the wire.
func testAddDeclToStore(t *testing.T, s store.Store, agentID *identity.Identity, campfireID string, payload []byte, supersedes string) string {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if supersedes != "" {
		b, _ := json.Marshal(supersedes)
		raw["supersedes"] = b
	}
	finalPayload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, err := message.NewMessage(agentID.PrivateKey, agentID.PublicKey, finalPayload, []string{cfconvention.ConventionOperationTag}, nil)
	if err != nil {
		t.Fatalf("creating message: %v", err)
	}
	rec := store.MessageRecord{
		ID:         msg.ID,
		CampfireID: campfireID,
		Sender:     msg.SenderHex(),
		Payload:    msg.Payload,
		Tags:       msg.Tags,
		Timestamp:  msg.Timestamp,
		Signature:  msg.Signature,
	}
	if _, err := s.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	return msg.ID
}

// testOpenStore opens a test store in a temp directory.
func testOpenStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// testAgent generates a test identity.
func testAgent(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

// testClientFrom creates a *protocol.Client wrapping the given store and identity.
func testClientFrom(s store.Store, agentID *identity.Identity) *protocol.Client {
	return protocol.New(s, agentID)
}

// testMembership returns a minimal Membership for use with performSupersede
// in tests that do not exercise the send path (early-exit tests).
// creatorPubkey, when non-empty, sets the operator identity for the campfire.
func testMembership(campfireID string, creatorPubkey ...string) *store.Membership {
	m := &store.Membership{
		CampfireID:    campfireID,
		TransportDir:  "/tmp/dontguess-test-transport",
		JoinProtocol:  "open",
		Role:          "member",
		JoinedAt:      time.Now().UnixNano(),
		TransportType: "unknown", // non-filesystem → falls back to local store
	}
	if len(creatorPubkey) > 0 {
		m.CreatorPubkey = creatorPubkey[0]
	}
	return m
}

// testCampfireResult holds the result of testSetupCampfire.
type testCampfireResult struct {
	CampfireID string
	Membership *store.Membership
	Client     *protocol.Client
	Store      store.Store
}

// testSetupCampfire creates a real campfire with filesystem transport, registers
// agentID as a member, and returns a testCampfireResult with the campfire ID,
// membership, protocol.Client (backed by the real store), and the store itself
// (for listOperationsForRegistry, which still takes StoreReader).
func testSetupCampfire(t *testing.T, agentID *identity.Identity) *testCampfireResult {
	t.Helper()
	transportDir := t.TempDir()
	dbDir := t.TempDir()

	// Create a new campfire.
	cf, err := campfire.New("invite-only", nil, 1)
	if err != nil {
		t.Fatalf("creating campfire: %v", err)
	}
	cfID := cf.PublicKeyHex()

	// Initialize transport for the campfire.
	tr := fs.New(transportDir)
	if err := tr.Init(cf); err != nil {
		t.Fatalf("initializing transport: %v", err)
	}

	// Register agentID as a member in the transport.
	if err := tr.WriteMember(cfID, campfire.MemberRecord{
		PublicKey: agentID.PublicKey,
		JoinedAt:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("writing member: %v", err)
	}

	// Open store and record membership.
	s, err := store.Open(filepath.Join(dbDir, "store.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	transportCampfireDir := tr.CampfireDir(cfID)
	m := &store.Membership{
		CampfireID:   cfID,
		TransportDir: transportCampfireDir,
		JoinProtocol: cf.JoinProtocol,
		Role:         store.PeerRoleCreator,
		JoinedAt:     store.NowNano(),
		Threshold:    cf.Threshold,
	}
	if err := s.AddMembership(*m); err != nil {
		t.Fatalf("adding membership: %v", err)
	}

	client := protocol.New(s, agentID)

	return &testCampfireResult{
		CampfireID: cfID,
		Membership: m,
		Client:     client,
		Store:      s,
	}
}

// ---- tests ----

// TestSupersede_PromoteThenSupersede exercises the item spec's core scenario:
//  1. Promote put v0.1.
//  2. Supersede with put v0.2 (adds optional `priority` arg).
//  3. Verify agents see the updated schema — v0.1 gone, v0.2 present.
func TestSupersede_PromoteThenSupersede(t *testing.T) {
	agentID := testAgent(t)

	// Use a real campfire with transport so client.Send can deliver messages.
	harness := testSetupCampfire(t, agentID)
	client := harness.Client
	campfireID := harness.CampfireID

	// ── Stage 1: promote put v0.1 ──────────────────────────────────────────

	putV1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true, "max_length": 4096,
			"description": "What the cached inference does"},
		{"name": "content_hash", "type": "string", "required": true, "max_length": 128,
			"description": "SHA-256 hash of content"},
		{"name": "token_cost", "type": "integer", "required": true,
			"description": "Original inference cost in tokens"},
		{"name": "content_size", "type": "integer", "required": true,
			"description": "Size in bytes"},
	}
	putV1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", putV1Args)
	v1MsgID := testWriteDeclToStore(t, client, campfireID, putV1Payload, "")
	t.Logf("Stage 1: promoted put v0.1, msgID=%s", shortID(v1MsgID))

	// Verify v0.1 visible in ListOperations.
	decls, err := listOperationsForRegistry(harness.Store, campfireID)
	if err != nil {
		t.Fatalf("ListOperations after v1 promote: %v", err)
	}
	foundV1 := false
	for _, d := range decls {
		if d.MessageID == v1MsgID {
			foundV1 = true
		}
	}
	if !foundV1 {
		t.Fatalf("Stage 1: put v0.1 not found in ListOperations")
	}
	t.Logf("Stage 1 OK: put v0.1 visible in ListOperations (%d decls)", len(decls))

	// ── Stage 2: supersede with put v0.2 (adds optional priority arg) ──────

	putV2Args := append(putV1Args, map[string]any{
		"name":        "priority",
		"type":        "integer",
		"description": "Optional submission priority (1=highest). Exchange may use for queue ordering.",
	})
	putV2Payload := testBuildDecl("dontguess-exchange", "put", "0.2", putV2Args)

	// Write new declaration file for performSupersede to read.
	tmpDir := t.TempDir()
	v2File := filepath.Join(tmpDir, "put-v0.2.json")
	if err := os.WriteFile(v2File, putV2Payload, 0600); err != nil {
		t.Fatalf("writing v2 file: %v", err)
	}

	// Ensure the old message is findable by the client.
	oldMsg, err := client.Get(v1MsgID)
	if err != nil || oldMsg == nil {
		t.Fatalf("Stage 2: old message %s not found via client.Get: %v", shortID(v1MsgID), err)
	}

	result, err := performSupersede(
		v2File, putV2Payload,
		campfireID, v1MsgID,
		client, harness.Membership,
		false, // no --force
	)
	if err != nil {
		t.Fatalf("Stage 2: performSupersede returned error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("Stage 2: performSupersede failed: %s", result.Error)
	}
	v2MsgID := result.MessageID
	t.Logf("Stage 2: superseded with put v0.2, msgID=%s, changeKind=%s", shortID(v2MsgID), result.ChangeKind)

	// ── Stage 3: verify agents see updated schema ───────────────────────────

	declsAfter, err := listOperationsForRegistry(harness.Store, campfireID)
	if err != nil {
		t.Fatalf("Stage 3: ListOperations after supersede: %v", err)
	}

	// v0.1 must be gone (superseded).
	for _, d := range declsAfter {
		if d.MessageID == v1MsgID {
			t.Errorf("Stage 3: put v0.1 (msgID=%s) still present after supersede", shortID(v1MsgID))
		}
	}

	// v0.2 must be present with the priority arg.
	var v2Decl *cfconvention.Declaration
	for _, d := range declsAfter {
		if d.MessageID == v2MsgID {
			v2Decl = d
			break
		}
	}
	if v2Decl == nil {
		t.Fatalf("Stage 3: put v0.2 (msgID=%s) not found in ListOperations", shortID(v2MsgID))
	}

	// Verify priority arg is present in the v0.2 schema.
	foundPriority := false
	for _, arg := range v2Decl.Args {
		if arg.Name == "priority" {
			foundPriority = true
		}
	}
	if !foundPriority {
		t.Errorf("Stage 3: priority arg not present in put v0.2 schema (got %d args)", len(v2Decl.Args))
	}

	t.Logf("Stage 3 OK: put v0.2 visible in ListOperations with priority arg (%d decls after)", len(declsAfter))
	t.Logf("All stages passed: promote v0.1 → supersede v0.2 → agents see updated schema")
}

// TestSupersede_BreakingChangeBlocked verifies that breaking changes are blocked
// without --force.
func TestSupersede_BreakingChangeBlocked(t *testing.T) {
	agentID := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agentID)

	cfID, _ := identity.Generate()
	campfireID := cfID.PublicKeyHex()

	// Write v0.1 directly to store — test exits before reaching sendSupersede,
	// so no transport is needed.
	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "domain", "type": "string", "description": "domain tag"},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", v1Args)
	v1ID := testAddDeclToStore(t, s, agentID, campfireID, v1Payload, "")

	// v2 removes the domain arg — breaking change.
	v2Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		// domain removed
	}
	v2Payload := testBuildDecl("dontguess-exchange", "put", "1.0", v2Args) // major bump

	tmpDir := t.TempDir()
	v2File := filepath.Join(tmpDir, "put-v1.0.json")
	if err := os.WriteFile(v2File, v2Payload, 0600); err != nil {
		t.Fatalf("writing v2 file: %v", err)
	}

	result, err := performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		client, testMembership(campfireID),
		false, // no --force → should be blocked
	)
	if err != nil {
		t.Fatalf("performSupersede returned error: %v", err)
	}
	if result.Error == "" {
		t.Error("expected breaking change to be blocked without --force, but got no error")
	}
	t.Logf("breaking change blocked as expected: %s", result.Error)
}

// TestSupersede_BreakingChangeAllowedWithForce verifies that --force bypasses
// breaking-change validation.
func TestSupersede_BreakingChangeAllowedWithForce(t *testing.T) {
	agentID := testAgent(t)

	// Use a real campfire with transport so client.Send can deliver messages.
	harness := testSetupCampfire(t, agentID)
	client := harness.Client
	campfireID := harness.CampfireID

	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "domain", "type": "string"},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", v1Args)
	v1ID := testWriteDeclToStore(t, client, campfireID, v1Payload, "")

	// v2 removes domain — breaking, but --force.
	v2Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	v2Payload := testBuildDecl("dontguess-exchange", "put", "1.0", v2Args)

	tmpDir := t.TempDir()
	v2File := filepath.Join(tmpDir, "put-v1.0.json")
	if err := os.WriteFile(v2File, v2Payload, 0600); err != nil {
		t.Fatalf("writing v2 file: %v", err)
	}

	result, err := performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		client, harness.Membership,
		true, // --force
	)
	if err != nil {
		t.Fatalf("performSupersede returned error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("expected --force to allow breaking change, got: %s", result.Error)
	}
	if result.MessageID == "" {
		t.Error("expected a published message ID")
	}
	t.Logf("breaking change allowed with --force: msgID=%s", shortID(result.MessageID))

	// v0.1 must be superseded.
	decls, err := listOperationsForRegistry(harness.Store, campfireID)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	for _, d := range decls {
		if d.MessageID == v1ID {
			t.Error("v0.1 still visible after forced supersede")
		}
	}
	t.Logf("forced supersede OK: v0.1 gone, v1.0 present")
}

// TestSupersede_LintFailure verifies that a malformed new declaration is rejected.
func TestSupersede_LintFailure(t *testing.T) {
	agentID := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agentID)

	cfID, _ := identity.Generate()
	campfireID := cfID.PublicKeyHex()

	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", nil)
	v1ID := testAddDeclToStore(t, s, agentID, campfireID, v1Payload, "")

	// Malformed JSON.
	invalidPayload := []byte(`{not json}`)
	tmpDir := t.TempDir()
	badFile := filepath.Join(tmpDir, "bad.json")
	if err := os.WriteFile(badFile, invalidPayload, 0600); err != nil {
		t.Fatalf("writing bad file: %v", err)
	}

	result, err := performSupersede(
		badFile, invalidPayload,
		campfireID, v1ID,
		client, testMembership(campfireID),
		false,
	)
	if err != nil {
		t.Fatalf("performSupersede returned error: %v", err)
	}
	if result.Error == "" {
		t.Error("expected lint failure for malformed JSON, got no error")
	}
	t.Logf("lint failure detected: %s", result.Error)
}

// TestSupersede_VersionBumpTooSmall verifies that adding an optional arg with
// only a patch bump is rejected.
func TestSupersede_VersionBumpTooSmall(t *testing.T) {
	agentID := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agentID)

	cfID, _ := identity.Generate()
	campfireID := cfID.PublicKeyHex()

	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1.0", v1Args)
	v1ID := testAddDeclToStore(t, s, agentID, campfireID, v1Payload, "")

	// v2 adds optional arg but only bumps patch — wrong.
	v2Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "priority", "type": "integer"},
	}
	v2Payload := testBuildDecl("dontguess-exchange", "put", "0.1.1", v2Args) // patch, not minor

	tmpDir := t.TempDir()
	v2File := filepath.Join(tmpDir, "put-v0.1.1.json")
	if err := os.WriteFile(v2File, v2Payload, 0600); err != nil {
		t.Fatalf("writing v2 file: %v", err)
	}

	result, err := performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		client, testMembership(campfireID),
		false,
	)
	if err != nil {
		t.Fatalf("performSupersede returned error: %v", err)
	}
	if result.Error == "" {
		t.Error("expected version bump error for patch-only bump with optional arg addition")
	}
	t.Logf("version bump validation caught: %s", result.Error)
}

// TestSupersede_NonOperatorRejected verifies that a non-operator campfire member
// cannot supersede a convention. Only the campfire creator (operator) is allowed.
func TestSupersede_NonOperatorRejected(t *testing.T) {
	operatorID := testAgent(t)
	nonOperatorID := testAgent(t)

	// Use a real campfire with transport (operator is the creator/member).
	harness := testSetupCampfire(t, operatorID)
	campfireID := harness.CampfireID

	// Build a non-operator client backed by the same store.
	nonOperatorClient := protocol.New(harness.Store, nonOperatorID)

	// Set CreatorPubkey so the operator check works correctly.
	m := &store.Membership{
		CampfireID:    campfireID,
		TransportDir:  harness.Membership.TransportDir,
		JoinProtocol:  harness.Membership.JoinProtocol,
		Role:          harness.Membership.Role,
		JoinedAt:      harness.Membership.JoinedAt,
		Threshold:     harness.Membership.Threshold,
		CreatorPubkey: operatorID.PublicKeyHex(),
	}

	// Promote v0.1 as the operator.
	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", v1Args)
	v1ID := testWriteDeclToStore(t, harness.Client, campfireID, v1Payload, "")

	// v2 adds an optional arg (valid minor bump).
	v2Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "priority", "type": "integer"},
	}
	v2Payload := testBuildDecl("dontguess-exchange", "put", "0.2", v2Args)

	tmpDir := t.TempDir()
	v2File := filepath.Join(tmpDir, "put-v0.2.json")
	if err := os.WriteFile(v2File, v2Payload, 0600); err != nil {
		t.Fatalf("writing v2 file: %v", err)
	}

	// Non-operator attempts to supersede — must be rejected before reaching send.
	result, err := performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		nonOperatorClient, m, // caller is nonOperatorClient, not operator
		false,
	)
	if err != nil {
		t.Fatalf("performSupersede returned unexpected error: %v", err)
	}
	if result.Error == "" {
		t.Fatal("expected non-operator supersede to be rejected, but got no error")
	}
	if result.MessageID != "" {
		t.Errorf("expected no message published, got msgID=%s", result.MessageID)
	}
	t.Logf("non-operator supersede rejected as expected: %s", result.Error)

	// Operator supersedes successfully using the real campfire harness.
	result, err = performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		harness.Client, m, // caller is operatorID via harness.Client
		false,
	)
	if err != nil {
		t.Fatalf("performSupersede returned error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("expected operator supersede to succeed, got: %s", result.Error)
	}
	t.Logf("operator supersede succeeded: msgID=%s", shortID(result.MessageID))
}

// TestInjectSupersedes verifies the supersedes field is correctly injected.
func TestInjectSupersedes(t *testing.T) {
	payload := []byte(`{"convention":"test","operation":"foo","version":"0.1"}`)
	msgID := "abc-123-456"

	out, err := injectSupersedes(payload, msgID)
	if err != nil {
		t.Fatalf("injectSupersedes failed: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var got string
	if err := json.Unmarshal(result["supersedes"], &got); err != nil {
		t.Fatalf("unmarshal supersedes field: %v", err)
	}
	if got != msgID {
		t.Errorf("expected supersedes=%s, got %s", msgID, got)
	}
}
