// Package librelay defines the wire contract between a contenox runtime and a
// relay: the [Frame] envelope, its NDJSON codec ([Reader], [Writer]), and the
// relay-level control messages. A runtime dials out and holds the connection;
// nothing here listens, and nothing here knows a hostname — the endpoint and
// the relay's public key are configuration.
//
// The package exists at the module root rather than under internal/ because a
// relay implementation is a separate Go module and cannot import internal/.
// Both ends must compile against one definition of the envelope; two
// definitions that drift is the failure this placement prevents.
//
// # The envelope
//
// A frame carries routing ([Frame.Instance], [Frame.Session]), a discriminator
// ([Frame.Type]), correlation ([Frame.ID] / [Frame.ReplyTo]) and an opaque
// [Frame.Payload]. The payload is json.RawMessage precisely so a relay can
// route a frame without parsing what is inside it; a relay that unmarshals ACP
// has taken on a dependency the design says it does not have.
//
// There is no transport encryption layer here. That is decided: the relay reads
// frames and TLS provides confidentiality, so the envelope is plain readable
// JSON with no sealed blob and no split between routing header and body.
//
// # One type space
//
// Control and tunnelled traffic share [Frame.Type] rather than living in two
// fields. A receiver's read loop then makes exactly one decision per frame:
// [ControlPrefix] means "for me", anything else means "route it". Two fields
// can disagree with each other and every disagreement is a case somebody has to
// invent behavior for.
//
// # Compatibility
//
// Unknown is never fatal, in both directions:
//
//   - Unknown fields are ignored (the decoder does not use DisallowUnknownFields),
//     so the envelope evolves by addition only. A field may never change meaning.
//   - An unknown non-control type is opaque and gets routed on (instance,
//     session) as any other tunnelled frame would.
//   - An unknown control type is answered by [Unsupported] when it is a request
//     and dropped when it is not. A request always gets exactly one reply, so a
//     newer peer talking to an older one fails fast instead of blocking on a
//     response that will never come; a response or notification never induces a
//     reply, so two peers cannot ping-pong errors at each other.
//
// The protocol version is negotiated once in [Hello] / [Welcome] and is
// deliberately not a per-frame field: it would cost bytes on every frame to
// carry a number that cannot change mid-connection, and it could not help
// anyway — a framing change severe enough to need it is a change that makes the
// frame unparseable before the version is reachable.
//
// # Framing
//
// NDJSON, the same framing ACP uses. The payload is already a JSON value, so a
// connector splices an ACP line into a frame with a byte copy and no re-encode,
// and neither end links a second parser. [Writer] compacts the payload, which
// is what makes newline-delimiting safe: a raw newline between JSON tokens in a
// caller-supplied payload would otherwise split one frame into two.
package librelay
