package libacp

// NegotiateProtocolVersion returns the protocol version two peers will speak:
// theirs when this peer can speak it (1 <= theirs <= ours), otherwise ours
// (normally ProtocolVersion). Deliberately does not require exact equality —
// a peer answering a different, mutually supported version is spec-legal,
// not an interop failure.
func NegotiateProtocolVersion(theirs, ours int) int {
	if theirs >= 1 && theirs <= ours {
		return theirs
	}
	return ours
}
