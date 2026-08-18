package sessionvitals

import (
	"time"

	libacp "github.com/contenox/contenox/libacp"
)

// notifiableStopReasons are the turn endings worth reclaiming the operator's
// attention for: the turn is genuinely over. A cancelled turn is deliberately
// absent — the operator cancelled it, they are already here.
var notifiableStopReasons = map[libacp.StopReason]bool{
	libacp.StopReasonEndTurn:         true,
	libacp.StopReasonMaxTokens:       true,
	libacp.StopReasonMaxTurnRequests: true,
	libacp.StopReasonRefusal:         true,
}

// NotifiableStop reports whether a turn ending with reason is worth an alert.
func NotifiableStop(reason libacp.StopReason) bool { return notifiableStopReasons[reason] }

// notifiableReportKinds are the mission-report kinds that mean "a human is
// needed or the work is done" — progress pings never alert.
var notifiableReportKinds = map[string]bool{
	"blocker": true,
	"result":  true,
}

// NotifiableReport reports whether a mission report of kind is worth an
// alert.
func NotifiableReport(kind string) bool { return notifiableReportKinds[kind] }

// defaultAlertWindow is the rate floor NewAlerter uses when given zero.
const defaultAlertWindow = 2 * time.Second

// Alerter is the decision "should the operator be interrupted right now",
// held apart from how an interruption is delivered — a terminal rings a BEL,
// a window raises a notification, and both must suppress and coalesce
// identically. It carries no clock and no output; Ring only answers.
// Not safe for concurrent use.
type Alerter struct {
	window time.Duration
	last   time.Time
	has    bool
}

// NewAlerter returns an Alerter that emits at most one alert per window, or
// per defaultAlertWindow when window is zero or negative.
func NewAlerter(window time.Duration) *Alerter {
	if window <= 0 {
		window = defaultAlertWindow
	}
	return &Alerter{window: window}
}

// Ring reports whether an alert should be delivered at now, and records it
// when so. It is suppressed while the surface has the operator's focus (they
// are already looking) unless always is set, which is reserved for facts that
// BLOCK work — an unanswered permission gate, a mission ask, an inbox arrival
// no session was watching. Whatever the suppression rule, at most one alert
// lands per window however many notifiable facts arrive inside it.
func (a *Alerter) Ring(now time.Time, focused, always bool) bool {
	if !always && focused {
		return false
	}
	if a.has && now.Sub(a.last) < a.window {
		return false
	}
	a.has = true
	a.last = now
	return true
}
