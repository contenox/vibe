// Package nativeturn is the serve-level survival layer for native ACP chain
// turns: it runs an in-flight turn on a serve-rooted Registry, off any single
// connection, so a dropped connection detaches a viewer without cancelling
// the turn. Anti-zombie guarantees are enforced by four belts: a last-viewer
// grace timer, a hard per-turn deadline, a bounded replay journal, and a
// periodic reaper backstop. Every exported type is safe for concurrent use.
package nativeturn

import (
	"fmt"
	"strings"
	"time"
)

// Config holds the survival-layer tunables. TurnDeadline and GraceWindow are
// operator-facing (env-backed, see ParseEnv); JournalSize is overridable only
// in-process (tests).
type Config struct {
	// TurnDeadline is the hard wall-clock ceiling on a single turn (belt 2).
	// A turn still running when it elapses is terminated with a context
	// deadline. Must be > 0.
	TurnDeadline time.Duration
	// GraceWindow is how long an in-flight turn survives with no viewer
	// attached before it is cancelled and torn down (belt 1); a reattach
	// inside the window keeps it alive. Must be > 0.
	GraceWindow time.Duration
	// JournalSize bounds the per-session replay journal (belt 3): the most
	// recent JournalSize events are retained, dropped oldest-first. Zero or
	// negative resolves to DefaultJournalSize in New.
	JournalSize int
}

const (
	// DefaultTurnDeadline is the fallback hard turn ceiling (belt 2).
	//
	// Sized for the turn this runtime actually runs: dozens of tool calls, a
	// large diff read back, and a hosted model that thinks. Fifteen minutes
	// killed working turns, and a turn killed at the ceiling loses everything
	// it had done — the cost of being wrong here is asymmetric, so the ceiling
	// is set where only a wedged turn reaches it.
	DefaultTurnDeadline = 60 * time.Minute
	// DefaultGraceWindow is the fallback last-viewer-detach survival window (belt 1).
	//
	// This is anti-zombie, not a deadline: belts 2 and 4 already bound how long
	// an unattended turn can live, so belt 1 does not need to be the tight one.
	// At 60s a closed laptop lid, a suspended terminal, a VPN reconnect or an
	// editor restart destroyed an in-flight turn the operator fully intended to
	// come back to.
	DefaultGraceWindow = 15 * time.Minute
	// DefaultJournalSize bounds the per-session replay journal (belt 3).
	DefaultJournalSize = 512
)

// ParseEnv builds a Config from the raw CONTENOX_TURN_MAX (hard deadline) and
// CONTENOX_TURN_GRACE (last-viewer grace) env strings. An empty field takes
// the corresponding default; a value must be a positive Go duration (e.g.
// "15m", "90s"). JournalSize is not env-configurable.
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
