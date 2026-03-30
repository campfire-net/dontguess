package exchange_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

func loadIdentity(cfHome string) (*identity.Identity, error) {
	return identity.Load(filepath.Join(cfHome, "identity.json"))
}

func TestInit_CreatesNamedViews(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Read all messages from transport, filter for campfire:view.
	transport := fs.New(transportDir)
	msgs, err := transport.ListMessages(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	viewNames := make(map[string]bool)
	for _, msg := range msgs {
		hasViewTag := false
		for _, tag := range msg.Tags {
			if tag == "campfire:view" {
				hasViewTag = true
				break
			}
		}
		if !hasViewTag {
			continue
		}
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

	expected := []string{"puts", "put-accepts", "buys", "match-results", "settlements", "disputes"}
	for _, name := range expected {
		if !viewNames[name] {
			t.Errorf("expected view %q to be created, got views: %v", name, viewNames)
		}
	}
}

func TestEnsureViews_Idempotent(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Sync transport into store so EnsureViews can see existing views.
	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer st.Close()

	transport := fs.New(transportDir)
	msgs, err := transport.ListMessages(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("listing transport messages: %v", err)
	}
	for i := range msgs {
		rec := store.MessageRecordFromMessage(cfg.ExchangeCampfireID, &msgs[i], store.NowNano())
		st.AddMessage(rec) //nolint:errcheck
	}

	// Load operator identity for the second EnsureViews call.
	ident, err := loadIdentity(cfHome)
	if err != nil {
		t.Fatalf("loading identity: %v", err)
	}

	// Second call should create zero views (all already exist).
	created, err := exchange.EnsureViews(cfg.ExchangeCampfireID, ident, st, transport)
	if err != nil {
		t.Fatalf("EnsureViews: %v", err)
	}
	if created != 0 {
		t.Errorf("expected 0 views created on second call, got %d", created)
	}
}
