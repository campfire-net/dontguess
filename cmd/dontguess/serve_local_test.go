package main

// serve_local_test.go — feature tests for dontguess-275 (standalone
// local-only cache: no campfire relay, no campfire identity, no scrip
// network dependency).
//
// TestServeLocal_PutBuyMatch_NoCampfire is the ground-source proof: it
// builds the exact same pieces runServeLocal wires together
// (loadOrCreateLocalOperatorKey + a real pkg/store.Store + exchange.NewEngine
// with only CampfireID/LocalStore/OperatorPublicKey set) and drives a real
// put -> put-accept -> buy -> match flow through eng.Start, with NO
// protocol.Client, NO ReadClient/WriteClient, NO ScripStore, NO
// ProvenanceChecker anywhere in this file — nothing here can reach a
// campfire even if one existed.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// TestLoadOrCreateLocalOperatorKey_Persists verifies the local operator key
// is generated once and reused on subsequent calls (and process restarts),
// so state.OperatorKey — and therefore Sender on every locally-emitted
// operator message — stays stable across `dontguess serve --local` restarts.
func TestLoadOrCreateLocalOperatorKey_Persists(t *testing.T) {
	dgHome := t.TempDir()

	key1, err := loadOrCreateLocalOperatorKey(dgHome)
	if err != nil {
		t.Fatalf("first loadOrCreateLocalOperatorKey: %v", err)
	}
	if len(key1) != 32 { // 16 random bytes, hex-encoded
		t.Fatalf("key length = %d, want 32 (16 random bytes hex-encoded)", len(key1))
	}

	key2, err := loadOrCreateLocalOperatorKey(dgHome)
	if err != nil {
		t.Fatalf("second loadOrCreateLocalOperatorKey: %v", err)
	}
	if key1 != key2 {
		t.Fatalf("key not persisted: first=%s second=%s", key1, key2)
	}
}

// localPutPayload builds a minimal valid exchange:put payload with content
// sized to satisfy the content-size plausibility check (dontguess-46f).
func localPutPayload(desc string, tokenCost int64) []byte {
	prefix := []byte("cached inference result: " + desc + " ")
	size := int(tokenCost/exchange.MaxTokensPerByte) + 1024
	contentBytes := make([]byte, size)
	copy(contentBytes, prefix)
	for i := len(prefix); i < size; i++ {
		contentBytes[i] = byte('a' + i%26)
	}
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString(contentBytes),
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	})
	return p
}

func localBuyPayload(task string, budget int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"task":        task,
		"budget":      budget,
		"max_results": 3,
	})
	return p
}

func randomLocalMsgID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestServeLocal_PutBuyMatch_NoCampfire drives a full put -> put-accept ->
// buy -> match cycle through the exact pieces runServeLocal wires together,
// proving the standalone local-only outcome (dontguess-275): a single agent
// can put/buy with zero campfire relay, identity, or scrip network
// dependency. No protocol.Client is constructed anywhere in this file.
func TestServeLocal_PutBuyMatch_NoCampfire(t *testing.T) {
	dgHome := t.TempDir()

	operatorKey, err := loadOrCreateLocalOperatorKey(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateLocalOperatorKey: %v", err)
	}

	ls, err := dgstore.Open(filepath.Join(dgHome, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		PollInterval:      20 * time.Millisecond,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	sellerKey := randomLocalMsgID(t)
	putID := randomLocalMsgID(t)
	if err := ls.Append(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     sellerKey,
		Payload:    localPutPayload("Go HTTP handler unit test generator", 8000),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("appending put: %v", err)
	}

	// AutoAcceptPut works with zero campfire: WriteClient is nil, so
	// sendOperatorMessage routes through sendLocalOperatorMessage, appending
	// the put-accept directly to LocalStore.
	if err := eng.AutoAcceptPut(putID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	if got := len(eng.State().Inventory()); got != 1 {
		t.Fatalf("expected 1 inventory entry after accept, got %d", got)
	}

	buyerKey := randomLocalMsgID(t)
	buyID := randomLocalMsgID(t)
	if err := ls.Append(dgstore.Record{
		ID:         buyID,
		CampfireID: "local",
		Sender:     buyerKey,
		Payload:    localBuyPayload("Generate unit tests for a Go HTTP handler", 50000),
		Tags:       []string{exchange.TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("appending buy: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(2 * time.Second)
	var sawMatch bool
	for time.Now().Before(deadline) {
		msgs, err := ls.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		for _, m := range msgs {
			for _, tag := range m.Tags {
				if tag == exchange.TagMatch {
					sawMatch = true
				}
			}
		}
		if sawMatch {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if !sawMatch {
		t.Fatal("no exchange:match record appeared in the local store — put/buy/match did not complete with zero campfire")
	}
}
