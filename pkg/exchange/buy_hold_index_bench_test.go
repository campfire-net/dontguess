package exchange_test

// BenchmarkFindExistingBuyerAcceptHold measures the O(1) index lookup for
// finding an existing scrip-buy-hold reservation, given a match message ID.
//
// Before dontguess-xn9: findExistingBuyerAcceptHold called store.ListMessages
// with a tag filter and iterated over all matching results — O(n) in the number
// of buy-hold messages on the campfire log.
//
// After dontguess-xn9: GetBuyHoldReservation reads from State.matchToBuyHold,
// a map populated during Replay and Apply. O(1) lookup regardless of log size.
//
// To compare:
//   go test ./pkg/exchange/... -run=^$ -bench=BenchmarkFindExistingBuyerAcceptHold -benchmem

import (
	"encoding/json"
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// BenchmarkGetBuyHoldReservation_IndexLookup benchmarks the O(1) state-index
// lookup path used after dontguess-xn9.
//
// Populates a State with N scrip-buy-hold messages via Replay, then benchmarks
// the GetBuyHoldReservation call for the last match ID.
func BenchmarkGetBuyHoldReservation_IndexLookup(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		b.Run(
			itoa(n)+"_holds",
			func(b *testing.B) {
				s := exchange.NewState()
				msgs := makeBuyHoldMessages(b, n)
				s.Replay(msgs)
				lastMatchID := "match-msg-" + itoa(n-1)
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = s.GetBuyHoldReservation(lastMatchID)
				}
			},
		)
	}
}

// BenchmarkGetBuyHoldReservation_Miss benchmarks the case where the match ID
// is not in the index (cache miss). Should still be O(1).
func BenchmarkGetBuyHoldReservation_Miss(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(
			itoa(n)+"_holds_miss",
			func(b *testing.B) {
				s := exchange.NewState()
				msgs := makeBuyHoldMessages(b, n)
				s.Replay(msgs)
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = s.GetBuyHoldReservation("no-such-match-id")
				}
			},
		)
	}
}

// makeBuyHoldMessages builds n synthetic scrip-buy-hold Messages.
// Each uses a unique matchMsgID "match-msg-{i}" and reservationID "res-{i}".
func makeBuyHoldMessages(tb testing.TB, n int) []exchange.Message {
	tb.Helper()
	msgs := make([]exchange.Message, n)
	for i := 0; i < n; i++ {
		matchID := "match-msg-" + itoa(i)
		resID := "res-" + itoa(i)
		payload := scrip.BuyHoldPayload{
			Buyer:         "buyer-key-" + itoa(i),
			Amount:        100,
			Price:         90,
			Fee:           10,
			ReservationID: resID,
			BuyMsg:        matchID,
			ExpiresAt:     "2099-01-01T00:00:00Z",
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			tb.Fatalf("marshal BuyHoldPayload: %v", err)
		}
		msgs[i] = exchange.Message{
			ID:      "buy-hold-msg-" + itoa(i),
			Payload: raw,
			Tags:    []string{scrip.TagScripBuyHold},
		}
	}
	return msgs
}

// itoa is a minimal int-to-string for benchmark naming (avoids fmt import).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
