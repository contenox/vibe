package term

import "github.com/contenox/contenox/internal/surfaces/beam/input"

// newParser is the single point where the engine binds to the real input
// decoder. Everything else in this package talks to eventParser, which is
// what keeps the renderer's tests free of both terminals and byte fixtures.
func newParser() eventParser { return input.NewParser() }
