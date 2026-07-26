// Package nativeturn is the serve-level survival layer for NATIVE ACP chain
// turns. A native turn (the contenox task-chain engine driving one session/prompt)
// historically ran inside the per-connection acpsvc.Transport, on a context that
// descended from the client's WebSocket request — so a dropped connection cancelled
// the running chain and wiped the work. This package relocates ownership of the
// in-flight turn OFF any single connection onto a serve-rooted Registry, exactly as
// agentinstance.Manager already does for EXTERNAL agents: the turn runs on a
// serve-rooted context, its events are captured in a bounded per-session journal,
// and per-connection Transports attach as thin VIEWERS that replay the journal on
// (re)attach and join the live fan-out. A viewer leaving (a connection drop) detaches
// but never cancels the turn.
//
// Anti-zombie is a hard requirement, enforced by four belts:
//
//   - Belt 1 (grace): when the LAST viewer detaches, a grace timer starts; a
//     reattach within the window cancels it, expiry cancels the turn and tears it
//     down (see turnSession.detach / attach).
//   - Belt 2 (hard deadline): every turn runs under a WithDeadline context bounded
//     by Config.TurnDeadline, so a runaway turn is always terminated (see
//     Registry.newTurnSession).
//   - Belt 3 (bounded journal): the replay journal is a fixed-size ring, so a
//     long-running turn's event history never grows without bound (see journal).
//   - Belt 4 (reaper): a periodic sweep tears down sessions past their grace or
//     hard deadline, and cleans up finished-and-unwatched turns, as a backstop for
//     the timer-driven paths (see Registry.ReapIdle).
//
// Everything here is safe for concurrent use. The Registry is generic over the
// turn's WORK: acpsvc supplies a TurnFunc that runs the chain and emits
// session/update notifications, so this package depends only on libacp and never
// on taskengine — the same policy-free split agentinstance keeps between its kernel
// and the service layer above it.
package nativeturn

import (
	"fmt"
	"strings"
	"time"
)

// Config holds the survival-layer tunables. TurnDeadline and GraceWindow are
// operator-facing (env-backed, see ParseEnv); JournalSize is a fixed structural
// bound overridable only in-process (tests), mirroring how terminalservice keeps
// scrollback size out of its env surface.
type Config struct {
	// TurnDeadline is the hard wall-clock ceiling on a single turn (Belt 2). A turn
	// still running when it elapses is terminated with a context deadline, which the
	// chain engine surfaces as a failure the reattaching client can see. Must be > 0.
	TurnDeadline time.Duration
	// GraceWindow is how long an in-flight turn survives with NO viewer attached
	// before it is cancelled and torn down (Belt 1). A reattach inside the window
	// keeps it alive. It also bounds how long a finished-but-still-watched turn and
	// a past-deadline turn linger before the reaper reclaims them. Must be > 0.
	GraceWindow time.Duration
	// JournalSize bounds the per-session replay journal (Belt 3): the most recent
	// JournalSize session/update events are retained for replay to a newly-attached
	// viewer, dropped oldest-first. Zero or negative resolves to DefaultJournalSize
	// in New.
	JournalSize int
}

const (
	// DefaultTurnDeadline is the fallback hard turn ceiling (Belt 2). Fifteen
	// minutes comfortably covers a long agentic chain while still bounding a runaway.
	DefaultTurnDeadline = 15 * time.Minute
	// DefaultGraceWindow is the fallback last-viewer-detach survival window (Belt 1).
	// A browser reload/reconnect completes well inside a minute.
	DefaultGraceWindow = 60 * time.Second
	// DefaultJournalSize bounds the per-session replay journal (Belt 3) — the
	// structured-event equivalent of a terminal's bounded scrollback. It mirrors
	// agentinstance's journal bound so both survival layers retain the same tail.
	DefaultJournalSize = 512
)

// ParseEnv builds a Config from the raw CONTENOX_TURN_MAX (hard deadline) and
// CONTENOX_TURN_GRACE (last-viewer grace) env strings. An empty field takes the
// corresponding default; a value must be a positive Go duration (e.g. "15m",
// "90s"). It mirrors terminalservice.ParseEnv's validation style so serve's config
// surface stays uniform. JournalSize is not env-configurable and is left at zero
// here (New defaults it).
func ParseEnv(turnMax, turnGrace string) (Config, error) {
	cfg := Config{TurnDeadline: DefaultTurnDeadline, GraceWindow: DefaultGraceWindow}

	if raw := strings.TrimSpace(turnMax); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("nativeturn: invalid CONTENOX_TURN_MAX %q: must be a positive Go duration (e.g. 15m)", turnMax)
		}
		cfg.TurnDeadline = d
	}

	if raw := strings.TrimSpace(turnGrace); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("nativeturn: invalid CONTENOX_TURN_GRACE %q: must be a positive Go duration (e.g. 60s)", turnGrace)
		}
		cfg.GraceWindow = d
	}

	return cfg, nil
}
