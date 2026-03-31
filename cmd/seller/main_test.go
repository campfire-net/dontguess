package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestSellerPut_ContentField verifies that buildPutPayload produces a payload
// with a 'content' field (carrying the actual cached inference text) and no
// 'content_hash' field, satisfying dontguess-3c4.
func TestSellerPut_ContentField(t *testing.T) {
	t.Parallel()

	spec := putSpec{
		Description: "test description",
		Content:     "test result",
		TokenCost:   100,
		ContentType: "code",
		ContentSize: 50,
		Domains:     []string{"go"},
		Tags:        []string{"exchange:put"},
	}

	payload := buildPutPayload(spec)

	wantContent := base64.StdEncoding.EncodeToString([]byte("test result"))

	// Must have 'content' field with the base64-encoded value.
	content, ok := payload["content"]
	if !ok {
		t.Fatal("payload missing 'content' field")
	}
	if content != wantContent {
		t.Errorf("payload content = %q, want base64 %q", content, wantContent)
	}

	// Must NOT have 'content_hash' field.
	if _, hasHash := payload["content_hash"]; hasHash {
		t.Error("payload must not contain 'content_hash' field")
	}

	// Verify round-trip through JSON (the actual wire format).
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if _, hasHash := decoded["content_hash"]; hasHash {
		t.Error("JSON-encoded payload must not contain 'content_hash' field")
	}
	if decoded["content"] != wantContent {
		t.Errorf("JSON-decoded content = %v, want base64 %q", decoded["content"], wantContent)
	}
}

// TestSellerPut_ContentField_EmptyContent verifies that an empty content string
// is transmitted as-is (not omitted), preserving the field contract.
func TestSellerPut_ContentField_EmptyContent(t *testing.T) {
	t.Parallel()

	spec := putSpec{
		Description: "empty content test",
		Content:     "",
		TokenCost:   10,
		ContentType: "analysis",
		ContentSize: 0,
		Domains:     []string{"test"},
	}

	payload := buildPutPayload(spec)

	if _, ok := payload["content"]; !ok {
		t.Fatal("payload missing 'content' field even when empty")
	}
	if _, hasHash := payload["content_hash"]; hasHash {
		t.Error("payload must not contain 'content_hash' field")
	}
}
