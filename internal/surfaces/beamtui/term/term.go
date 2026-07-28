// Package term owns the terminal: the only beam package allowed to read
// terminal state or write to the tty. Everything else exchanges pure data
// through the Engine interface — frame.Frame out, input.Event in. Content
// is appended once to real scrollback and never repainted; only the bounded
// Live region at the bottom is managed. The engine never enables the
// alternate screen or captures the mouse.
package term

import (
	"github.com/contenox/contenox/internal/surfaces/beamtui/frame"
	"github.com/contenox/contenox/internal/surfaces/beamtui/input"
)

// StyleResolver turns a semantic StyleID into the SGR prefix/suffix pair
// for the detected terminal tier. The style package provides the only
// production implementation; tests use identity or tagging resolvers.
type StyleResolver interface {
	SGR(id frame.StyleID) (prefix, suffix string)
}

// Engine is the seam between beam and a real terminal. Exactly one
// implementation runs per process; tests substitute fakes.
//
// Engine methods are not safe for concurrent use: the app-shell select
// loop is the single caller, which is what makes frame corruption from
// interleaved writes impossible by construction.
type Engine interface {
	// Events yields decoded input events. The channel closes when input
	// ends or the engine is closed.
	Events() <-chan input.Event

	// Commit renders one frame: Scrollback lines are appended to terminal
	// history exactly once, then the Live region is repainted in place
	// (minimally — unchanged rows are not rewritten), inside a
	// synchronized-output bracket so partial frames are never visible.
	Commit(f frame.Frame) error

	// Size reports the current terminal size in cells.
	Size() (width, height int)

	// Suspend restores cooked mode and the cursor, runs fn with full use
	// of the terminal (e.g. $EDITOR, the setup wizard), then re-enters raw
	// mode, emits a fresh ResizeEvent, and forces the next Commit to fully
	// repaint the Live region.
	Suspend(fn func() error) error

	// Bell emits BEL, the always-safe completion signal.
	Bell()

	// CopyToClipboard writes text to the system clipboard via OSC 52,
	// DCS-wrapped under tmux/screen. Fire-and-forget: nil error means
	// "sent", never "copied". Payloads beyond the cap are truncated and
	// reported so the UI can say so.
	CopyToClipboard(text string) (truncated bool, err error)

	// Close restores the terminal fully (cooked mode, cursor visible,
	// bracketed paste and focus reporting off). Idempotent and safe from
	// deferred panic handlers.
	Close() error
}
