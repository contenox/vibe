package libacp

// NegotiateProtocolVersion returns the protocol version two peers will speak:
// theirs when this peer can speak it (1 <= theirs <= ours), otherwise ours,
// without requiring exact equality.
func NegotiateProtocolVersion(theirs, ours int) int {
	if theirs >= 1 && theirs <= ours {
		return theirs
	}
	return ours
}
