package relayclient

// Enforcement proof for dontguess-4e3 finding C (TEAM tier) — the exclusive-sender
// filter in FetchOpenAssigns (assign.go): an assign whose ExclusiveSender is set to
// agent X must be EXCLUDED from agent Y's listing, INCLUDED in X's listing, and an
// OPEN assign (ExclusiveSender=="") must be included for everyone.
//
// Before this test, that filter branch (rec.ExclusiveSender != "" &&
// rec.ExclusiveSender != agentPubKeyHex) had NO coverage with a mismatching caller
// key. The test drives the REAL FetchOpenAssigns over a REAL relay.Conn against an
// in-process fake relay that serves genuinely-signed operator assign events — no
// mock of the fold/filter under test.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/relay"
)

// buildSignedAssign renders a genuinely signed exchange:assign event authored by
// operator, carrying task_type / reward / exclusive_sender. Mirrors
// buildSignedPutReject's proto.Message -> nostr.ToNostrEvent -> identity.Event ->
// SignEvent pattern. Panics on fixture error (runs from the fake relay goroutine
// where t.Fatalf is unsafe).
func buildSignedAssign(operator identity.Signer, taskType, exclusiveSender string, reward int64) *identity.Event {
	payload, err := json.Marshal(map[string]any{
		"task_type":        taskType,
		"reward":           reward,
		"exclusive_sender": exclusiveSender,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal assign payload: %v", err))
	}
	msg := &proto.Message{
		Sender:      operator.PubKeyHex(),
		Payload:     payload,
		Tags:        []string{exchange.TagAssign},
		Antecedents: []string{},
		Timestamp:   time.Now().UnixNano(),
	}
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		panic(fmt.Sprintf("ToNostrEvent: %v", err))
	}
	ev := &identity.Event{
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(operator, ev); err != nil {
		panic(fmt.Sprintf("SignEvent: %v", err))
	}
	return ev
}

// assignServingWSConn is an in-process fake relay.WSConn that, on any REQ, replays
// a fixed set of pre-signed assign events (as SubEvent frames) followed by EOSE for
// the REQ's subscription id — exactly what FetchOpenAssigns' fetchAllAssignEvents
// consumes. EVENT writes (none are sent by FetchOpenAssigns) are OK-acked.
type assignServingWSConn struct {
	mu     sync.Mutex
	recv   chan []byte
	closed chan struct{}
	once   sync.Once
	events []*identity.Event
}

func newAssignServingWSConn(events []*identity.Event) *assignServingWSConn {
	return &assignServingWSConn{
		recv:   make(chan []byte, 64),
		closed: make(chan struct{}),
		events: events,
	}
}

func (c *assignServingWSConn) WriteMessage(_ int, data []byte) error {
	f, err := relay.ParseFrame(data)
	if err != nil {
		return nil
	}
	switch f.Type {
	case relay.LabelEVENT:
		if f.Event != nil {
			ok, _ := relay.EncodeOK(f.Event.ID, true, "")
			c.push(ok)
		}
	case relay.LabelREQ:
		for _, ev := range c.events {
			frame, encErr := relay.EncodeSubEvent(f.SubID, ev)
			if encErr != nil {
				continue
			}
			c.push(frame)
		}
		eose, _ := relay.EncodeEOSE(f.SubID)
		c.push(eose)
	}
	return nil
}

func (c *assignServingWSConn) push(b []byte) {
	select {
	case c.recv <- b:
	case <-c.closed:
	}
}

func (c *assignServingWSConn) ReadMessage() (int, []byte, error) {
	select {
	case b := <-c.recv:
		return 1, b, nil
	case <-c.closed:
		return 0, nil, fmt.Errorf("assign serving ws: closed")
	}
}

func (c *assignServingWSConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// fetchAssignsFor runs the REAL FetchOpenAssigns for callerPubKeyHex against a fresh
// fake relay serving events, signed by operator.
func fetchAssignsFor(t *testing.T, operator identity.Signer, events []*identity.Event, callerPubKeyHex string) []OpenAssign {
	t.Helper()
	ws := newAssignServingWSConn(events)
	caller := newSigner(t)
	conn := NewConn("ws://fake", caller, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := FetchOpenAssigns(ctx, conn, operator.PubKeyHex(), callerPubKeyHex)
	if err != nil {
		t.Fatalf("FetchOpenAssigns(caller=%s): %v", callerPubKeyHex[:8], err)
	}
	return out
}

// hasTaskType reports whether any listed assign has the given task_type.
func hasTaskType(assigns []OpenAssign, taskType string) bool {
	for _, a := range assigns {
		if a.TaskType == taskType {
			return true
		}
	}
	return false
}

// TestFetchOpenAssigns_ExclusiveSenderFilter_TeamTier proves the exclusive-sender
// filter on the team tier for BOTH a matching and a mismatching caller key, plus
// the open-to-everyone case.
func TestFetchOpenAssigns_ExclusiveSenderFilter_TeamTier(t *testing.T) {
	t.Parallel()

	operator := newSigner(t)
	agentX := newSigner(t)
	agentY := newSigner(t)

	// One assign EXCLUSIVE to agent X, one OPEN to everyone.
	events := []*identity.Event{
		buildSignedAssign(operator, "exclusive-task", agentX.PubKeyHex(), 500),
		buildSignedAssign(operator, "open-task", "", 300),
	}

	// Agent X: sees BOTH the exclusive (targeted at X) and the open assign.
	xList := fetchAssignsFor(t, operator, events, agentX.PubKeyHex())
	if !hasTaskType(xList, "exclusive-task") {
		t.Errorf("agent X listing MISSING its own exclusive assign; got %+v", xList)
	}
	if !hasTaskType(xList, "open-task") {
		t.Errorf("agent X listing missing the open assign; got %+v", xList)
	}

	// Agent Y: the exclusive-to-X assign is EXCLUDED; the open assign is included.
	yList := fetchAssignsFor(t, operator, events, agentY.PubKeyHex())
	if hasTaskType(yList, "exclusive-task") {
		t.Errorf("agent Y listing INCLUDED an assign exclusive to agent X (filter not enforced); got %+v", yList)
	}
	if !hasTaskType(yList, "open-task") {
		t.Errorf("agent Y listing missing the open assign (open assigns must be visible to everyone); got %+v", yList)
	}

	// The exclusive assign that agent Y sees excluded is exactly the one agent X
	// sees included, and it carries X's key as ExclusiveSender.
	var exclusiveForX *OpenAssign
	for i := range xList {
		if xList[i].TaskType == "exclusive-task" {
			exclusiveForX = &xList[i]
		}
	}
	if exclusiveForX == nil {
		t.Fatalf("could not locate the exclusive assign in agent X's listing")
	}
	if exclusiveForX.ExclusiveSender != agentX.PubKeyHex() {
		t.Errorf("exclusive assign ExclusiveSender = %q, want agent X's key %q", exclusiveForX.ExclusiveSender, agentX.PubKeyHex())
	}
}
