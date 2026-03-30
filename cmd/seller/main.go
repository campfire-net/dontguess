package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"
)

const (
	exchangeCampfireID = "c5c1eee98996231b1c292ab87ec193ead370ff88dfb2cfbb8423834da1b4812c"
	operatorKey        = "8205ae6fe3af5c3b4688e7c53a38c45efe2362d64be250929c56ae7d0d16b398"
)

type putSpec struct {
	Description string
	ContentHash string
	TokenCost   int
	ContentType string
	ContentSize int
	Domains     []string
	Tags        []string
}

type settlePayload struct {
	Phase    string `json:"phase"`
	EntryID  string `json:"entry_id"`
	Price    int64  `json:"price"`
	ExpireAt string `json:"expires_at"`
}

func main() {
	// Load or generate seller identity.
	identityPath := os.Getenv("SELLER_IDENTITY")
	if identityPath == "" {
		home, _ := os.UserHomeDir()
		identityPath = filepath.Join(home, ".clankeros", "automata", "dontguess-seller", "identity.json")
	}

	var sellerID *identity.Identity
	if identity.Exists(identityPath) {
		id, err := identity.Load(identityPath)
		if err != nil {
			log.Fatalf("loading seller identity: %v", err)
		}
		sellerID = id
		fmt.Printf("Loaded seller identity: %s\n", sellerID.PublicKeyHex())
	} else {
		// Ensure directory exists.
		if err := os.MkdirAll(filepath.Dir(identityPath), 0o700); err != nil {
			log.Fatalf("creating identity dir: %v", err)
		}
		id, err := identity.Generate()
		if err != nil {
			log.Fatalf("generating seller identity: %v", err)
		}
		if err := id.Save(identityPath); err != nil {
			log.Fatalf("saving seller identity: %v", err)
		}
		sellerID = id
		fmt.Printf("Generated new seller identity: %s\n", sellerID.PublicKeyHex())
	}

	// Set up filesystem transport.
	transportPath := os.Getenv("EXCHANGE_TRANSPORT")
	if transportPath == "" {
		transportPath = "/tmp/campfire"
	}
	transport := fs.New(transportPath)

	// Open the SQLite store.
	cfHome := os.Getenv("CF_HOME")
	if cfHome == "" {
		home, _ := os.UserHomeDir()
		cfHome = filepath.Join(home, ".campfire")
	}
	storePath := store.StorePath(cfHome)
	s, err := store.Open(storePath)
	if err != nil {
		log.Fatalf("opening store: %v", err)
	}
	defer s.Close()

	// Define 3 puts.
	puts := []putSpec{
		{
			Description: "Cached analysis of Go concurrency patterns for web servers. Covers goroutine pools, channel patterns, context propagation, and graceful shutdown. 2500 tokens of inference.",
			ContentHash: "sha256:a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			TokenCost:   2500,
			ContentType: "analysis",
			ContentSize: 12000,
			Domains:     []string{"go", "concurrency"},
			Tags:        []string{"exchange:put", "exchange:content-type:analysis", "exchange:domain:go"},
		},
		{
			Description: "Performance benchmark results for PostgreSQL vs SQLite for time-series data at 10K-100K rows. Includes query latency, write throughput, and memory usage comparisons. 4000 tokens of inference.",
			ContentHash: "sha256:b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3",
			TokenCost:   4000,
			ContentType: "data",
			ContentSize: 8500,
			Domains:     []string{"database", "performance"},
			Tags:        []string{"exchange:put", "exchange:content-type:data", "exchange:domain:database"},
		},
		{
			Description: "Rate limiter implementation in Go using token bucket algorithm. Production-ready with burst support, per-key limits, and Redis backend option. 1500 tokens of inference.",
			ContentHash: "sha256:c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
			TokenCost:   1500,
			ContentType: "code",
			ContentSize: 6000,
			Domains:     []string{"go", "networking"},
			Tags:        []string{"exchange:put", "exchange:content-type:code", "exchange:domain:go"},
		},
	}

	// Register seller as a campfire member so protocol.Client.Send can verify membership.
	// This is equivalent to a lightweight join — write the member record to the transport
	// and record membership in the local store.
	if err := transport.WriteMember(exchangeCampfireID, campfire.MemberRecord{
		PublicKey: sellerID.PublicKey,
		JoinedAt:  time.Now().UnixNano(),
	}); err != nil {
		// Non-fatal: may already be registered from a previous run.
		fmt.Fprintf(os.Stderr, "warning: writing seller member record: %v\n", err)
	}
	transportDir := transport.CampfireDir(exchangeCampfireID)
	if err := s.AddMembership(store.Membership{
		CampfireID:   exchangeCampfireID,
		TransportDir: transportDir,
		JoinProtocol: "open",
		Role:         store.PeerRoleMember,
		JoinedAt:     store.NowNano(),
	}); err != nil {
		// Non-fatal: may already be recorded.
		fmt.Fprintf(os.Stderr, "warning: recording exchange membership: %v\n", err)
	}

	// Build protocol client for sending puts.
	client := protocol.New(s, sellerID)

	// Send all 3 puts and collect message IDs.
	putMsgIDs := make([]string, 0, 3)
	expectedPrices := make([]int64, 0, 3)

	for i, p := range puts {
		payload := map[string]interface{}{
			"description":  p.Description,
			"content_hash": p.ContentHash,
			"token_cost":   p.TokenCost,
			"content_type": p.ContentType,
			"content_size": p.ContentSize,
			"domains":      p.Domains,
		}
		msgID, err := sendPut(client, payload, p.Tags)
		if err != nil {
			log.Fatalf("sending put %d: %v", i+1, err)
		}
		putMsgIDs = append(putMsgIDs, msgID)
		expectedPrices = append(expectedPrices, int64(p.TokenCost)*70/100)
		fmt.Printf("Put %d sent: msgID=%s (content_type=%s, token_cost=%d, expected_price=%d)\n",
			i+1, msgID, p.ContentType, p.TokenCost, int64(p.TokenCost)*70/100)
	}

	fmt.Println("\nAll 3 puts sent. Waiting for auto-accept responses...")

	// Poll for settle:put-accept responses (up to 15s).
	accepted := make(map[string]*settlePayload)
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) && len(accepted) < 3 {
		msgs, err := transport.ListMessages(exchangeCampfireID)
		if err != nil {
			log.Fatalf("reading messages: %v", err)
		}

		for _, msg := range msgs {
			// Look for settle messages with put-accept phase.
			hasSettle := false
			hasPutAccept := false
			for _, tag := range msg.Tags {
				if tag == "exchange:settle" {
					hasSettle = true
				}
				if tag == "exchange:phase:put-accept" {
					hasPutAccept = true
				}
			}
			if !hasSettle || !hasPutAccept {
				continue
			}

			var sp settlePayload
			if err := json.Unmarshal(msg.Payload, &sp); err != nil {
				continue
			}

			// Check if this settle references one of our puts.
			for _, putID := range putMsgIDs {
				if sp.EntryID == putID {
					if _, already := accepted[putID]; !already {
						accepted[putID] = &sp
					}
				}
			}
		}

		if len(accepted) < 3 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Report results.
	fmt.Printf("\n=== Results ===\n")
	allPass := true
	for i, putID := range putMsgIDs {
		sp, ok := accepted[putID]
		if !ok {
			fmt.Printf("FAIL: Put %d (%s) — no put-accept response received\n", i+1, puts[i].ContentType)
			allPass = false
			continue
		}

		if sp.Phase != "put-accept" {
			fmt.Printf("FAIL: Put %d (%s) — wrong phase: %s\n", i+1, puts[i].ContentType, sp.Phase)
			allPass = false
			continue
		}

		if sp.Price != expectedPrices[i] {
			fmt.Printf("FAIL: Put %d (%s) — price=%d, expected=%d\n", i+1, puts[i].ContentType, sp.Price, expectedPrices[i])
			allPass = false
			continue
		}

		fmt.Printf("PASS: Put %d (%s) — accepted, price=%d (70%% of %d)\n",
			i+1, puts[i].ContentType, sp.Price, puts[i].TokenCost)
	}

	if !allPass {
		fmt.Println("\nSome puts were not accepted within the timeout.")
		os.Exit(1)
	}

	fmt.Println("\nAll 3 puts accepted by exchange engine. Seller inventory complete.")
}

func sendPut(client *protocol.Client, payload interface{}, tags []string) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}
	msg, err := client.Send(protocol.SendRequest{
		CampfireID: exchangeCampfireID,
		Payload:    payloadBytes,
		Tags:       tags,
	})
	if err != nil {
		return "", fmt.Errorf("sending put: %w", err)
	}
	return msg.ID, nil
}

