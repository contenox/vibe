package eventlog

import "strings"

// MatchesType reports whether a listener pattern selects typeName. A pattern
// ending in ".*" matches every type under that dotted prefix; anything else is
// an exact match. A bare "*" is deliberately not a wildcard.
func MatchesType(pattern, typeName string) bool {
	pattern = strings.TrimSpace(pattern)
	typeName = strings.TrimSpace(typeName)
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		return prefix != "" && strings.HasPrefix(typeName, prefix+".")
	}
	return pattern == typeName
}

// IsPattern reports whether p is a prefix pattern rather than one exact type.
// Bus subjects are exact strings, so a pattern has no live subscription.
func IsPattern(p string) bool {
	return strings.HasSuffix(strings.TrimSpace(p), ".*")
}
