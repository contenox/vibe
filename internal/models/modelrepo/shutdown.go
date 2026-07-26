package modelrepo

import "sync"

// Shutdown hook registry. Backend packages that hold process-lifetime resources
// (e.g. native in-process inference sessions) register a cleanup function from
// init() — the same self-registration pattern as RegisterCatalogProvider — so
// the runtime can drain them generically without importing any concrete backend.

var (
	shutdownHooksMu sync.Mutex
	shutdownHooks   []func() error
)

// RegisterShutdownHook registers fn to be run by Shutdown. It is intended to be
// called from a backend package's init(). A nil fn is ignored.
func RegisterShutdownHook(fn func() error) {
	if fn == nil {
		return
	}
	shutdownHooksMu.Lock()
	defer shutdownHooksMu.Unlock()
	shutdownHooks = append(shutdownHooks, fn)
}

// Shutdown runs every registered shutdown hook and returns the first error, if
// any. All hooks run even if an earlier one fails. It is safe to call when no
// hooks are registered.
func Shutdown() error {
	shutdownHooksMu.Lock()
	hooks := make([]func() error, len(shutdownHooks))
	copy(hooks, shutdownHooks)
	shutdownHooksMu.Unlock()

	var first error
	for _, fn := range hooks {
		if err := fn(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
