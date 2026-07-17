package nativebert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tokenizerJSONPath resolves the on-disk tokenizer.json used to build the
// tokenizer under test. The full tokenizer.json is committed under testdata/
// (466 KB) so this parity test runs hermetically in CI without any download.
func tokenizerJSONPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("testdata", "tokenizer.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("committed fixture missing: %s: %v", p, err)
	}
	return p
}

func TestWordPieceMatchesHFGolden(t *testing.T) {
	tjBytes, err := os.ReadFile(tokenizerJSONPath(t))
	if err != nil {
		t.Fatalf("read tokenizer.json: %v", err)
	}
	wp, err := newWordPiece(tjBytes)
	if err != nil {
		t.Fatalf("newWordPiece: %v", err)
	}

	goldenBytes, err := os.ReadFile("testdata/tokenizer_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		Cases []struct {
			Text string `json:"text"`
			IDs  []int  `json:"ids"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	for _, c := range golden.Cases {
		got := wp.encode(c.Text)
		if !reflect.DeepEqual(got, c.IDs) {
			t.Errorf("encode(%q):\n got  %v\n want %v", c.Text, got, c.IDs)
		}
	}
}
