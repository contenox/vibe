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
// ⚠ "Ignored" holds for an endpoint, which decodes a frame and acts on it. It
// does NOT hold for a relay, which decodes a frame and RE-ENCODES it to forward:
// there an unknown field is not ignored, it is destroyed, and silently, because
// nothing on either side can see that it went missing. A relay must therefore be
// built against a version of this package at least as new as the endpoints it
// carries. Adding a field here is consequently a two-module change — the field,
// then the relay's dependency on it — and shipping only the first half is
// indistinguishable from the feature not working.
//
// The protocol version is negotiated once in [Hello] / [Welcome] and is
// deliberately not a per-frame field: it would cost bytes on every frame to
// carry a number that cannot change mid-connection, and it could not help
// anyway — a framing change severe enough to need it is a change that makes the
// frame unparseable before the version is reachable.
//
// # Authentication
//
// The handshake is mutual but asymmetric, because the two directions have
// different problems. The instance proves itself to the relay with a bearer
// credential on the transport's upgrade request — never in a frame, so nothing
// here handles it. The relay proves itself to the instance inside the
// handshake: [Hello] carries a fresh [Hello.Nonce] and [Welcome] answers with
// [Welcome.Signature], checked by [VerifyWelcome] against the public key the
// instance stored when it paired. Both ends compute the signed bytes with
// [SigningInput], which is the reason it lives in this shared module.
//
// # Resumption
//
// A dropped connection costs latency, never content. [Frame.Seq] is the
// producer's per-session cursor; on reconnect the receiver sends [Resume] with
// the last value it saw and the producer continues after it. That is SSE's
// model — one cursor per connection rather than an acknowledgement per message
// — and it is why nothing here carries delivery guarantees.
//
// Resumption is not relay control traffic. The producer replays because it is
// the side still holding the content, so [TypeResume] routes end to end and a
// relay treats it as any other cargo. It also travels one way only: replaying a
// command is not resumption, since a re-delivered instruction is a second
// instruction.
//
// # Framing
//
// NDJSON, the same framing ACP uses. The payload is already a JSON value, so a
// connector splices an ACP line into a frame with a byte copy and no re-encode,
// and neither end links a second parser. [Writer] compacts the payload, which
// is what makes newline-delimiting safe: a raw newline between JSON tokens in a
// caller-supplied payload would otherwise split one frame into two.
package librelay
