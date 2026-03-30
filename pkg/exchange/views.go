package exchange

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"
)

// viewDefinition matches the cf CLI's campfire:view message payload schema.
type viewDefinition struct {
	Name       string   `json:"name"`
	Predicate  string   `json:"predicate"`
	Projection []string `json:"projection,omitempty"`
	Ordering   string   `json:"ordering,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Refresh    string   `json:"refresh,omitempty"`
}

// StandardViews returns the exchange's named views. These use tag-based
// predicates that work with cf v0.10.7. When cf gains has-fulfillment support,
// these can be upgraded to match the convention spec §8 predicates.
func StandardViews() []viewDefinition {
	return []viewDefinition{
		{
			Name:      "puts",
			Predicate: `(tag "exchange:put")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name:      "put-accepts",
			Predicate: `(tag "exchange:phase:put-accept")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name:      "buys",
			Predicate: `(tag "exchange:buy")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name:      "match-results",
			Predicate: `(tag "exchange:match")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name:      "settlements",
			Predicate: `(tag "exchange:settle")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name:      "disputes",
			Predicate: `(and (tag "exchange:settle") (tag "exchange:phase:dispute"))`,
			Ordering:  "timestamp asc",
			Refresh:   "on-read",
		},
	}
}

// EnsureViews idempotently creates the standard named views on the exchange
// campfire. It checks which views already exist (by scanning campfire:view
// messages in the store) and creates only missing ones.
func EnsureViews(campfireID string, agentID *identity.Identity, st store.Store, transport *fs.Transport) (created int, err error) {
	existing, err := existingViewNames(st, campfireID)
	if err != nil {
		return 0, fmt.Errorf("listing existing views: %w", err)
	}

	for _, v := range StandardViews() {
		if existing[v.Name] {
			continue
		}
		if err := createView(campfireID, v, agentID, transport); err != nil {
			fmt.Fprintf(os.Stderr, "warning: creating view %q: %v\n", v.Name, err)
			continue
		}
		created++
	}
	return created, nil
}

// existingViewNames returns the set of view names already defined on the
// campfire by scanning campfire:view tagged messages in the store.
func existingViewNames(st store.Store, campfireID string) (map[string]bool, error) {
	storeRecs, err := st.ListMessages(campfireID, 0, store.MessageFilter{Tags: []string{"campfire:view"}})
	if err != nil {
		return nil, err
	}

	// Convert at the cf boundary to dontguess-owned Message type.
	msgs := FromStoreRecords(storeRecs)

	// Latest definition per name wins (later messages override earlier).
	names := make(map[string]bool)
	for _, m := range msgs {
		var def viewDefinition
		if err := json.Unmarshal(m.Payload, &def); err != nil {
			continue
		}
		names[def.Name] = true
	}
	return names, nil
}

// createView sends a campfire:view message to the transport.
func createView(campfireID string, def viewDefinition, agentID *identity.Identity, transport *fs.Transport) error {
	payload, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encoding view %q: %w", def.Name, err)
	}

	// sendViewMessage reuses the same message-creation pattern as
	// sendConventionMessage but with the campfire:view tag.
	return sendViewMessage(campfireID, payload, agentID, transport)
}

// sendViewMessage creates, signs, and writes a campfire:view message.
func sendViewMessage(campfireID string, payload []byte, agentID *identity.Identity, transport *fs.Transport) error {
	return sendTaggedMessage(campfireID, payload, []string{"campfire:view"}, agentID, transport)
}
