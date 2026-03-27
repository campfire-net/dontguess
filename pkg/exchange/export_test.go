package exchange

import "github.com/campfire-net/campfire/pkg/store"

// DispatchForTest exposes the engine's dispatch method for use in tests.
// It allows tests to trigger specific handler paths (settle, dispute) without
// running the full engine event loop.
func (e *Engine) DispatchForTest(msg *store.MessageRecord) error {
	return e.dispatch(msg)
}
