package exchange

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/campfire-net/campfire/pkg/protocol"
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
		{
			Name:      "assigns",
			Predicate: `(tag "exchange:assign")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{Name: "assign-claims", Predicate: `(tag "exchange:assign-claim")`, Ordering: "timestamp desc", Refresh: "on-read"},
		{Name: "assign-completes", Predicate: `(tag "exchange:assign-complete")`, Ordering: "timestamp desc", Refresh: "on-read"},
		{Name: "assign-accepts", Predicate: `(tag "exchange:assign-accept")`, Ordering: "timestamp desc", Refresh: "on-read"},
		{
			Name:      "scrip-assign-pay",
			Predicate: `(tag "dontguess:scrip-assign-pay")`,
			Ordering:  "timestamp desc",
			Refresh:   "on-read",
		},
		{
			Name: "messages",
			Predicate: `(or (tag "exchange:put") (tag "exchange:buy") (tag "exchange:match") ` +
				`(tag "exchange:settle") (tag "exchange:assign") (tag "exchange:dispute") ` +
				`(tag "exchange:phase:put-accept") (tag "exchange:phase:buy-complete") ` +
				`(tag "dontguess:scrip-assign-pay") (tag "dontguess:scrip-mint"))`,
			Ordering: "timestamp asc",
			Refresh:  "on-read",
		},
	}
}

// EnsureViews idempotently creates the standard named views on the exchange
// campfire. It checks which views already exist (by reading campfire:view
// messages via the client) and creates only missing ones.
func EnsureViews(campfireID string, client *protocol.Client) (created int, err error) {
	existing, err := existingViewNames(client, campfireID)
	if err != nil {
		return 0, fmt.Errorf("listing existing views: %w", err)
	}

	for _, v := range StandardViews() {
		if existing[v.Name] {
			continue
		}
		if err := createView(campfireID, v, client); err != nil {
			fmt.Fprintf(os.Stderr, "warning: creating view %q: %v\n", v.Name, err)
			continue
		}
		created++
	}
	return created, nil
}

// existingViewNames returns the set of view names already defined on the
// campfire by reading campfire:view tagged messages via the client.
func existingViewNames(client *protocol.Client, campfireID string) (map[string]bool, error) {
	result, err := client.Read(protocol.ReadRequest{
		CampfireID: campfireID,
		Tags:       []string{"campfire:view"},
	})
	if err != nil {
		return nil, err
	}

	// Latest definition per name wins (later messages override earlier).
	names := make(map[string]bool)
	for _, m := range result.Messages {
		var def viewDefinition
		if err := json.Unmarshal(m.Payload, &def); err != nil {
			continue
		}
		names[def.Name] = true
	}
	return names, nil
}

// createView sends a campfire:view message via client.Send.
func createView(campfireID string, def viewDefinition, client *protocol.Client) error {
	payload, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encoding view %q: %w", def.Name, err)
	}
	return sendViewMessage(campfireID, payload, client)
}

// sendViewMessage sends a campfire:view message via client.Send.
func sendViewMessage(campfireID string, payload []byte, client *protocol.Client) error {
	return sendTaggedMessage(campfireID, payload, []string{"campfire:view"}, client)
}
