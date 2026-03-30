package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cfconvention "github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
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
//
// The engine and convention code do not verify signatures, so we insert with
// an empty signature and a random message ID (same format as real messages).
func testAddDeclToStore(t *testing.T, s store.Store, senderPubKeyHex string, campfireID string, payload []byte, supersedes string) string {
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

	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatalf("generating message ID: %v", err)
	}
	msgID := hex.EncodeToString(idBytes)

	rec := store.MessageRecord{
		ID:          msgID,
		CampfireID:  campfireID,
		Sender:      senderPubKeyHex,
		Payload:     finalPayload,
		Tags:        []string{cfconvention.ConventionOperationTag},
		Antecedents: []string{},
		Timestamp:   store.NowNano(),
		Signature:   []byte{}, // non-nil required for NOT NULL BLOB constraint
	}
	if _, err := s.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	return msgID
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

// testAgentResult holds the result of testAgent.
type testAgentResult struct {
	Client *protocol.Client
	CfHome string // config dir — store is at CfHome/store.db
}

// testAgent creates a new protocol.Client with its own identity and store in a temp dir.
// Returns the client and its config directory. The client is closed on test cleanup.
func testAgent(t *testing.T) *testAgentResult {
	t.Helper()
	cfHome := t.TempDir()
	client, err := protocol.Init(cfHome)
	if err != nil {
		t.Fatalf("protocol.Init for test agent: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return &testAgentResult{Client: client, CfHome: cfHome}
}

// testRandomCampfireID returns a random 32-byte hex string for use as a fake campfire ID
// in tests that do not need a real campfire transport.
func testRandomCampfireID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating campfire ID: %v", err)
	}
	return hex.EncodeToString(b)
}

// testClientFrom creates a *protocol.Client wrapping the given store.
// Creates a read-only client (nil identity) — used for early-exit tests that
// check operator validation before reaching the send path.
func testClientFrom(s store.Store, _ *testAgentResult) *protocol.Client {
	return protocol.New(s, nil)
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

// testSetupCampfire creates a real campfire with filesystem transport using the
// protocol SDK. The agent is the campfire creator. Returns a testCampfireResult
// with the campfire ID, membership, protocol.Client, and the store.
//
// The Store field is a second connection to the same SQLite file as the agent's
// client, so that messages sent via client.Send are visible via Store.ListMessages.
func testSetupCampfire(t *testing.T, agent *testAgentResult) *testCampfireResult {
	t.Helper()
	transportDir := t.TempDir()
	agentClient := agent.Client

	// Create a new campfire via the SDK.
	createResult, err := agentClient.Create(protocol.CreateRequest{
		Transport: protocol.FilesystemTransport{
			Dir: transportDir,
		},
		Description:  "test campfire",
		JoinProtocol: "open",
		Threshold:    1,
	})
	if err != nil {
		t.Fatalf("creating campfire: %v", err)
	}
	cfID := createResult.CampfireID

	// Get the membership that was created by Create.
	mem, err := agentClient.GetMembership(cfID)
	if err != nil || mem == nil {
		t.Fatalf("getting membership after create: %v (mem=%v)", err, mem)
	}

	// Open a second connection to the same SQLite file used by agentClient.
	// SQLite allows multiple readers; messages written by agentClient.Send
	// are immediately visible to this connection.
	s, err := store.Open(store.StorePath(agent.CfHome))
	if err != nil {
		t.Fatalf("opening agent store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return &testCampfireResult{
		CampfireID: cfID,
		Membership: mem,
		Client:     agentClient,
		Store:      s,
	}
}

// ---- tests ----

// TestSupersede_PromoteThenSupersede exercises the item spec's core scenario:
//  1. Promote put v0.1.
//  2. Supersede with put v0.2 (adds optional `priority` arg).
//  3. Verify agents see the updated schema — v0.1 gone, v0.2 present.
func TestSupersede_PromoteThenSupersede(t *testing.T) {
	agent := testAgent(t)

	// Use a real campfire with transport so client.Send can deliver messages.
	harness := testSetupCampfire(t, agent)
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
	agent := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agent)
	senderKey := agent.Client.PublicKeyHex()

	campfireID := testRandomCampfireID(t)

	// Write v0.1 directly to store — test exits before reaching sendSupersede,
	// so no transport is needed.
	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
		{"name": "domain", "type": "string", "description": "domain tag"},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", v1Args)
	v1ID := testAddDeclToStore(t, s, senderKey, campfireID, v1Payload, "")

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
	agent := testAgent(t)

	// Use a real campfire with transport so client.Send can deliver messages.
	harness := testSetupCampfire(t, agent)
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
	agent := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agent)
	senderKey := agent.Client.PublicKeyHex()

	campfireID := testRandomCampfireID(t)

	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1", nil)
	v1ID := testAddDeclToStore(t, s, senderKey, campfireID, v1Payload, "")

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
	agent := testAgent(t)
	s := testOpenStore(t)
	client := testClientFrom(s, agent)
	senderKey := agent.Client.PublicKeyHex()

	campfireID := testRandomCampfireID(t)

	v1Args := []map[string]any{
		{"name": "description", "type": "string", "required": true},
	}
	v1Payload := testBuildDecl("dontguess-exchange", "put", "0.1.0", v1Args)
	v1ID := testAddDeclToStore(t, s, senderKey, campfireID, v1Payload, "")

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
	operatorAgent := testAgent(t)
	_ = testAgent(t) // nonOperatorAgent — not needed; nil-identity client simulates non-operator

	// Use a real campfire with transport (operator is the creator/member).
	harness := testSetupCampfire(t, operatorAgent)
	campfireID := harness.CampfireID

	// Set CreatorPubkey so the operator check works correctly.
	m := &store.Membership{
		CampfireID:    campfireID,
		TransportDir:  harness.Membership.TransportDir,
		JoinProtocol:  harness.Membership.JoinProtocol,
		Role:          harness.Membership.Role,
		JoinedAt:      harness.Membership.JoinedAt,
		Threshold:     harness.Membership.Threshold,
		CreatorPubkey: operatorAgent.Client.PublicKeyHex(),
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
	// Use a client with empty PublicKeyHex (nil identity) which won't match CreatorPubkey.
	nonOpReadClient := protocol.New(harness.Store, nil)
	result, err := performSupersede(
		v2File, v2Payload,
		campfireID, v1ID,
		nonOpReadClient, m, // caller has empty pubkey → not operator
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
		harness.Client, m, // caller is operatorClient via harness.Client
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
