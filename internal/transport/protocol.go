package transport

// ProtocolVersion is the modeld gRPC/session wire and semantics contract this
// build speaks. Bump it for breaking changes to transport requests, responses,
// or session behavior. MinProtocol is the oldest daemon protocol this build
// still accepts.
const ProtocolVersion = 1

const MinProtocol = 1

// Supported reports whether a daemon speaking protocol p is usable by this
// runtime build.
func Supported(p int) bool {
	return p >= MinProtocol && p <= ProtocolVersion
}
