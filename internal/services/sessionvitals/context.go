package sessionvitals

import "fmt"

// Pressure names how close a session is to filling its context window. It is
// a judgement, not a rendering: a terminal paints it as a colour and a window
// as a badge, but both must call the same occupancy "high".
type Pressure string

const (
	// PressureNone is "no usage_update has landed yet" — Size is zero and
	// every other reading would be a fabrication.
	PressureNone Pressure = "none"
	// PressureNormal is anything below pressureHighPercent.
	PressureNormal Pressure = "normal"
	// PressureHigh is at or above pressureHighPercent: worth showing, not
	// worth acting on.
	PressureHigh Pressure = "high"
	// PressureCritical is at or above pressureCriticalPercent: the next turn
	// may not fit.
	PressureCritical Pressure = "critical"
)

// The two thresholds Pressure steps at, in percent of the window. They are
// constants rather than knobs so every surface agrees on where "high" starts.
const (
	pressureHighPercent     = 75
	pressureCriticalPercent = 90
)

// ContextUsage is one session's context-window occupancy as last reported by
// the agent. A zero Size means "not reported yet", which is why Known exists:
// 0/0 is an absence of information, never an empty window.
type ContextUsage struct {
	Used int
	Size int
}

// Known reports whether a usage update has actually landed. A consumer must
// not render a gauge, a percentage, or a pressure colour while this is false.
func (u ContextUsage) Known() bool { return u.Size > 0 }

// Percent is Used as a whole percentage of Size, truncated, and 0 when the
// usage is not Known.
func (u ContextUsage) Percent() int {
	if !u.Known() {
		return 0
	}
	return u.Used * 100 / u.Size
}

// Pressure classifies Percent against the two thresholds, or PressureNone
// when the usage is not Known.
func (u ContextUsage) Pressure() Pressure {
	if !u.Known() {
		return PressureNone
	}
	switch pct := u.Percent(); {
	case pct >= pressureCriticalPercent:
		return PressureCritical
	case pct >= pressureHighPercent:
		return PressureHigh
	default:
		return PressureNormal
	}
}

// Text is the canonical "used/size (pct%)" reading every surface shows, so a
// window and a terminal never disagree on the numbers or their punctuation.
// It is empty when the usage is not Known.
func (u ContextUsage) Text() string {
	if !u.Known() {
		return ""
	}
	return fmt.Sprintf("%d/%d (%d%%)", u.Used, u.Size, u.Percent())
}
