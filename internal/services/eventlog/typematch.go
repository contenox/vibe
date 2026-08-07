package eventlog

import "strings"

// Listener type patterns. A trigger's listen_for.type is not always one exact
// string: it may name a dotted subtree, which is what lets a family of related
// producers be consumed by one trigger without enumerating every member.
//
// Kept deliberately free of any declared-catalog machinery: matching is a pure
// string decision the dispatcher can make with no registry, no database and no
// producer constructed.

// MatchesType reports whether a listener pattern selects typeName.
//
// A pattern ending in ".*" matches every type under that dotted prefix
// ("mission.unit.*" selects "mission.unit.started"); any other pattern is an
// exact string match, so every listener written before prefix matching existed
// keeps its original meaning. A bare "*" is deliberately NOT a wildcard: a
// listener must name the subtree it consumes, or the hop guard becomes the only
// thing standing between a trigger and its own output.
func MatchesType(pattern, typeName string) bool {
	pattern = strings.TrimSpace(pattern)
	typeName = strings.TrimSpace(typeName)
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok {
		return prefix != "" && strings.HasPrefix(typeName, prefix+".")
	}
	return pattern == typeName
}

// IsPattern reports whether p is a prefix pattern rather than one exact type.
// The dispatcher reads it to decide what it can subscribe to: bus subjects are
// exact strings, so a pattern has no live subscription and relies on the
// backstop poll.
func IsPattern(p string) bool {
	return strings.HasSuffix(strings.TrimSpace(p), ".*")
}
