package hitlservice

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// TimeoutIndefinite is Rule.TimeoutS for an ask with no deadline at all; 0 still means "unset", which falls to the configured ceiling.
const TimeoutIndefinite = -1

// WaitIndefinite is TimeoutIndefinite as a duration; any negative wait is one.
const WaitIndefinite = time.Duration(-1)

// FallbackApprovalCeiling bounds an ask no rule and no operator gave a wait: MaxRuleTimeoutS, the longest wait a rule may state.
const FallbackApprovalCeiling = MaxRuleTimeoutS * time.Second

var indefiniteSpellings = []string{"never", "forever", "indefinite"}

func IndefiniteSpellings() string { return strings.Join(indefiniteSpellings, ", ") }

func IsIndefiniteWord(s string) bool {
	return slices.Contains(indefiniteSpellings, strings.ToLower(strings.TrimSpace(s)))
}

func Indefinite(wait time.Duration) bool { return wait < 0 }

func WaitOf(timeoutS int) time.Duration {
	if timeoutS < 0 {
		return WaitIndefinite
	}
	return time.Duration(timeoutS) * time.Second
}

// ParseWait reads a wait as an operator writes one: a Go duration of whole seconds, or a word for no deadline.
func ParseWait(s string) (time.Duration, error) {
	raw := strings.TrimSpace(s)
	if IsIndefiniteWord(raw) {
		return WaitIndefinite, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a wait — write a duration the way Go writes one (90s, 30m, 2h), or one of %s for an ask that waits until it is answered", s, IndefiniteSpellings())
	}
	switch {
	case d < 0:
		return 0, fmt.Errorf("%q: a wait cannot run backwards; write one of %s for an ask with no deadline", s, IndefiniteSpellings())
	case d == 0:
		return 0, fmt.Errorf("%q states no wait at all; write one of %s for an ask that waits until it is answered", s, IndefiniteSpellings())
	case d%time.Second != 0:
		return 0, fmt.Errorf("%q carries sub-second precision, which would be truncated to %s — write the wait you mean", s, d.Truncate(time.Second))
	case d > MaxRuleTimeoutS*time.Second:
		return 0, fmt.Errorf("%q is longer than the %s a wait accepts — write one of %s rather than a number that means it", s, time.Duration(MaxRuleTimeoutS)*time.Second, IndefiniteSpellings())
	}
	return d, nil
}

// FormatWait is ParseWait's inverse, so a stored wait reads back as it was written.
func FormatWait(wait time.Duration) string {
	if Indefinite(wait) {
		return indefiniteSpellings[0]
	}
	return wait.String()
}
