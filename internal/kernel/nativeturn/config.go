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

// Config holds the survival-layer tunables; TurnDeadline and GraceWindow are
// operator-facing (env-backed, see ParseEnv), JournalSize is overridable only
// in-process (tests).
type Config struct {
	// TurnDeadline is the hard wall-clock ceiling on a single turn (belt 2),
	// terminating a still-running turn with a context deadline; must be > 0.
	TurnDeadline time.Duration
	// GraceWindow is how long an in-flight turn survives with no viewer attached
	// before it is cancelled (belt 1); a reattach inside the window keeps it
	// alive, and it must be > 0.
	GraceWindow time.Duration
	// JournalSize bounds the per-session replay journal (belt 3) to the most
	// recent events, oldest dropped first; zero or negative resolves to
	// DefaultJournalSize in New.
	JournalSize int
}

const (
	// DefaultTurnDeadline is the fallback hard turn ceiling (belt 2).
	DefaultTurnDeadline = 60 * time.Minute
	// DefaultGraceWindow is the fallback last-viewer-detach survival window (belt 1).
	DefaultGraceWindow = 15 * time.Minute
	// DefaultJournalSize bounds the per-session replay journal (belt 3).
	DefaultJournalSize = 512
)

// ParseEnv builds a Config from the raw CONTENOX_TURN_MAX (hard deadline) and
// CONTENOX_TURN_GRACE (last-viewer grace) env strings; an empty field takes the
// default, a set value must be a positive Go duration, and JournalSize is not
// env-configurable.
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
