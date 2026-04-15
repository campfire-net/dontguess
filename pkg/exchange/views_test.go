package exchange_test

import (
	"encoding/json"
	"testing"

	"github.com/campfire-net/campfire/pkg/protocol"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

func TestInit_CreatesNamedViews(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     cfHome,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	// Use protocol.Init to read messages via SDK.
	verifyClient, _, err := protocol.Init(cfHome)
	if err != nil {
		t.Fatalf("protocol.Init for verify: %v", err)
	}
	defer verifyClient.Close()

	readResult, err := verifyClient.Read(protocol.ReadRequest{
		CampfireID: cfg.ExchangeCampfireID,
		Tags:       []string{"campfire:view"},
	})
	if err != nil {
		t.Fatalf("Read view messages: %v", err)
	}
	msgs := readResult.Messages

	viewNames := make(map[string]bool)
	for _, msg := range msgs {
		var def struct {
			Name      string `json:"name"`
			Predicate string `json:"predicate"`
			Ordering  string `json:"ordering"`
		}
		if err := json.Unmarshal(msg.Payload, &def); err != nil {
			t.Errorf("parsing view message %s: %v", msg.ID, err)
			continue
		}
		viewNames[def.Name] = true
	}

	expected := []string{"puts", "put-accepts", "buys", "match-results", "settlements", "disputes", "assigns", "assign-claims", "assign-completes", "assign-accepts", "scrip-assign-pay", "messages"}
	for _, name := range expected {
		if !viewNames[name] {
			t.Errorf("expected view %q to be created, got views: %v", name, viewNames)
		}
	}
}

func TestViews_StandardViewsContainsAllExpected(t *testing.T) {
	t.Parallel()

	views := exchange.StandardViews()

	// Build a name→predicate map for easy lookup.
	byName := make(map[string]string)
	for _, v := range views {
		byName[v.Name] = v.Predicate
	}

	// Verify assigns view.
	predicate, ok := byName["assigns"]
	if !ok {
		t.Fatal("StandardViews() missing view \"assigns\"")
	}
	if predicate != `(tag "exchange:assign")` {
		t.Errorf("assigns predicate = %q, want %q", predicate, `(tag "exchange:assign")`)
	}

	// Verify messages view.
	predicate, ok = byName["messages"]
	if !ok {
		t.Fatal("StandardViews() missing view \"messages\"")
	}
	wantPredicate := `(or (tag "exchange:put") (tag "exchange:buy") (tag "exchange:match") ` +
		`(tag "exchange:settle") (tag "exchange:assign") (tag "exchange:dispute") ` +
		`(tag "exchange:phase:put-accept") (tag "exchange:phase:buy-complete") ` +
		`(tag "dontguess:scrip-assign-pay") (tag "dontguess:scrip-mint"))`
	if predicate != wantPredicate {
		t.Errorf("messages predicate = %q, want %q", predicate, wantPredicate)
	}

	// Verify total count: 9 original + 3 assign sub-operation views = 12.
	if len(views) != 12 {
		t.Errorf("StandardViews() returned %d views, want 12", len(views))
	}
}

func TestEnsureViews_Idempotent(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     cfHome,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	// Use protocol.Init to get a client; EnsureViews reads via client.Read.
	client, _, err := protocol.Init(cfHome)
	if err != nil {
		t.Fatalf("protocol.Init: %v", err)
	}
	defer client.Close()

	// Second call should create zero views (all already exist from Init).
	created, err := exchange.EnsureViews(cfg.ExchangeCampfireID, client)
	if err != nil {
		t.Fatalf("EnsureViews: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 views created on second call, got %d", created)
	}
}
