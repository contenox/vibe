package librelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ProtocolVersion is the envelope version this build speaks. It is exchanged
// once in [Hello] / [Welcome] and never appears on a frame; see the package
// doc for why.
const ProtocolVersion = 1

// Envelope limits. They bound what a decoder will allocate for a single frame
// before it gives up, so they are a memory contract and not merely a style
// rule. MaxFrameBytes sits below libacp's 64 MiB NDJSON ceiling on purpose: an
// ACP frame crosses a local pipe with one peer on it, whereas a relay
// multiplexes many instances and pays this buffer per connection.
const (
	MaxFrameBytes = 16 << 20
	MaxTypeBytes  = 128
	MaxIDBytes    = 256
)

// ControlPrefix marks a type as relay-level control traffic, addressed to the
// peer that receives it rather than to something behind it. It is the whole of
// the routing decision: a relay handles a frame whose type has this prefix and
// forwards every frame whose type does not, without ever looking at the
// payload.
const ControlPrefix = "relay."

// Control message types. Every one of these is part of the protocol floor and
// may never be removed, because [Unsupported] answers with [TypeError] and a
// peer that did not understand TypeError could not learn that it had failed.
const (
	// TypeHello is the connector's opening frame, payload [Hello]. It is a
	// request: the relay answers TypeWelcome or TypeError.
	TypeHello = ControlPrefix + "hello"
	// TypeWelcome is the relay's answer to TypeHello, payload [Welcome].
	TypeWelcome = ControlPrefix + "welcome"
	// TypeHeartbeat is a liveness probe with no payload, sent by either end.
	// It is a request; the peer answers TypeAck. Correlation matters here:
	// an ack that does not name the probe it answers cannot distinguish a
	// live peer from a stalled one that is one round behind.
	TypeHeartbeat = ControlPrefix + "heartbeat"
	// TypeAck answers TypeHeartbeat, carrying no payload.
	TypeAck = ControlPrefix + "ack"
	// TypeError reports a failure, payload [Error]. As a response it never
	// induces a response, which is what keeps two disagreeing peers from
	// exchanging errors forever.
	TypeError = ControlPrefix + "error"
)

// TypeACPMessage tunnels one ACP JSON-RPC message. The payload is whatever
// libacp put on the wire, byte for byte; a relay does not parse it and does
// not need libacp linked to route it.
const TypeACPMessage = "acp.message"

// Resumption types. Not control traffic: a relay routes them to the producer,
// which is the side holding the content to replay. See [Resume].
const (
	// TypeResume asks a session's producer to continue after a cursor. Always
	// a request, so it is always answered exactly once.
	TypeResume = "session.resume"
	// TypeResumed answers [TypeResume] and precedes the replayed frames.
	TypeResumed = "session.resumed"
)

// Error codes carried by [Error]. They are strings rather than integers so an
// unrecognized code degrades to something legible in a log.
const (
	CodeUnsupportedType = "unsupported_type"
	CodeMalformedFrame  = "malformed_frame"
	CodeUnknownInstance = "unknown_instance"
	CodeUnauthorized    = "unauthorized"
	CodeVersion         = "unsupported_version"
	// CodeCursorEvicted answers a [Resume] whose cursor the producer no
	// longer retains, when it cannot replay at all. A producer that can
	// replay part of the stream answers [Resumed] with Evicted set instead.
	CodeCursorEvicted = "cursor_evicted"
)

// Frame is the transport envelope. Field names on the wire are short because
// every ACP message pays for them.
//
// Instance and Session are the routing key and are direction-independent: they
// name the instance and session a frame concerns, not its sender. (instance,
// session) is the general case — one account may hold many instances, and a
// session identifier is only unique within its instance, so Session without
// Instance does not address anything.
type Frame struct {
	// Type discriminates the frame. Control types carry ControlPrefix; any
	// other value is opaque to a relay and gets routed.
	Type string `json:"type"`
	// Instance is the runtime instance this frame concerns. Empty only on
	// frames that precede identification.
	Instance string `json:"instance,omitempty"`
	// Session is the ACP session within Instance. Empty on control traffic
	// and on anything instance-scoped.
	Session string `json:"session,omitempty"`
	// ID marks this frame as a request and correlates the reply. Its
	// presence, not the type, is what obliges the receiver to answer
	// exactly once — which is why an unknown type is still answerable.
	ID string `json:"id,omitempty"`
	// ReplyTo carries the ID of the request being answered and marks this
	// frame as a response. Requests and responses use separate fields
	// rather than one shared id so direction is on the wire: a receiver
	// never has to guess whether it owes an answer, and therefore never
	// answers an answer.
	ReplyTo string `json:"re,omitempty"`
	// Seq is the producer's per-(Instance, Session) cursor for this frame,
	// monotonically increasing and gap-free within a session.
	//
	// It exists so a dropped connection costs latency rather than content:
	// the receiver remembers the last Seq it saw, and on reconnect asks the
	// producer to continue from there ([Resume]). That is SSE's model — a
	// cursor the receiver states once per connection, not an acknowledgement
	// per message — and it is the reason nothing here carries delivery
	// guarantees.
	//
	// Zero means unsequenced and is the correct value for control traffic and
	// for any producer that does not replay. A receiver must not infer
	// ordering from an unsequenced frame.
	//
	// Opaque to a relay, which routes on (Instance, Session) and never reads
	// this. Replay is the producer's, because the producer is the side that
	// still holds the content.
	Seq uint64 `json:"seq,omitempty"`
	// Payload is the message body, left as raw JSON so intermediaries do
	// not parse it. Empty is legal: heartbeat and ack carry nothing.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hello is the [TypeHello] payload: what the connector claims to be, before
// the relay has agreed to any of it.
type Hello struct {
	// ProtocolVersion is the highest envelope version the connector
	// speaks.
	ProtocolVersion int `json:"protocol_version"`
	// Instance identifies the runtime. It is also set on the frame, since
	// routing must not require reading a payload; the copy here is what a
	// relay checks the frame against.
	Instance string `json:"instance"`
	// Agent names the implementation and version, for operator diagnosis
	// only. Nothing may branch on it — that is what ProtocolVersion is for.
	Agent string `json:"agent,omitempty"`
	// Nonce is a fresh random challenge the relay signs into
	// [Welcome.Signature], so the connector can tell the relay it paired
	// with from anything else that answered the dial. It is generated per
	// connection by [NewNonce] and is never a secret — it is only ever
	// useful once. Empty means the connector is not asking to be convinced,
	// which a relay may still answer by signing nothing.
	Nonce []byte `json:"nonce,omitempty"`
}

// Welcome is the [TypeWelcome] payload: the relay's acceptance.
type Welcome struct {
	// ProtocolVersion is the version the relay has selected, which is
	// min(peer, self). The connector must check it against its own floor
	// and close if the relay chose lower than it can speak; a connection
	// held open on an unspeakable version is worse than no connection,
	// because it looks healthy.
	ProtocolVersion int `json:"protocol_version"`
	// Relay names the relay implementation and version, diagnostics only.
	Relay string `json:"relay,omitempty"`
	// Signature is Ed25519 over [SigningInput] of the connector's
	// [Hello.Nonce], the ProtocolVersion selected above and the instance —
	// the relay's half of a mutual authentication whose other half is a
	// bearer credential on the transport. It is omitempty because a
	// connector that pinned no key does not require one; a connector that
	// did treats its absence as fatal. See [VerifyWelcome].
	Signature []byte `json:"sig,omitempty"`
	// RetryAfterSeconds hints how long to wait before redialling if this
	// connection ends, so a draining relay can stagger a fleet's return
	// instead of taking the whole of it back at once. SSE's `retry:` field,
	// and it is a hint: a connector clamps it to its own bounds and never
	// lets a relay set an unbounded delay on itself.
	//
	// Zero means the connector's own backoff applies.
	RetryAfterSeconds int `json:"retry_after,omitempty"`
}

// Resume is the [TypeResume] payload: continue a session's stream after a
// cursor.
//
// It is NOT relay control traffic. The producer replays, so this is addressed
// end to end and a relay routes it like any other cargo — a relay that answered
// it would have to buffer session content, which is the design it does not
// have.
//
// One direction only, deliberately. Replaying commands is not resumption: a
// re-delivered "approve" is a second approval. Traffic that changes state
// carries its own idempotency and is never replayed by this mechanism.
type Resume struct {
	// AfterSeq is the last [Frame.Seq] the requester saw. Zero asks for the
	// whole retained stream, which is what a receiver that has seen nothing
	// sends.
	AfterSeq uint64 `json:"after_seq"`
}

// Resumed is the [TypeResumed] payload: what the producer is about to send.
type Resumed struct {
	// FromSeq is the first [Frame.Seq] that follows. Greater than
	// Resume.AfterSeq+1 only when Evicted is set.
	FromSeq uint64 `json:"from_seq"`
	// Evicted reports that the requested cursor is older than what the
	// producer retains, so frames between it and FromSeq are gone.
	//
	// Answered explicitly rather than by silently resuming from the oldest
	// retained frame. SSE does the latter and it is its one real defect: the
	// receiver cannot tell a continuous stream from one with a hole in it, so
	// a transcript silently loses turns. A receiver seeing this must refetch
	// state rather than append.
	Evicted bool `json:"evicted,omitempty"`
}

// Error is the [TypeError] payload.
type Error struct {
	// Code is machine-readable and stable; see the Code* constants.
	Code string `json:"code"`
	// Message is for humans and may change freely between versions.
	Message string `json:"message,omitempty"`
}

// Validation failures. They are all "this frame is not addressable", which a
// receiver treats as a bad message rather than a bad connection.
var (
	ErrEmptyType    = errors.New("librelay: frame type is empty")
	ErrTypeTooLong  = errors.New("librelay: frame type exceeds MaxTypeBytes")
	ErrIDTooLong    = errors.New("librelay: identifier exceeds MaxIDBytes")
	ErrControlChar  = errors.New("librelay: identifier contains a control character")
	ErrBothIDs      = errors.New("librelay: frame sets both id and re")
	ErrSessionAlone = errors.New("librelay: frame sets session without instance")
	ErrNotUTF8      = errors.New("librelay: identifier is not valid UTF-8")
)

// IsControl reports whether a type is relay-level control traffic. A relay
// handles these and forwards everything else, including types it has never
// heard of.
func IsControl(msgType string) bool { return strings.HasPrefix(msgType, ControlPrefix) }

// IsRequest reports whether f obliges the receiver to send exactly one
// response. Requests are identified by carrying an ID, not by their type, so
// this stays true for types this build does not know.
func (f Frame) IsRequest() bool { return f.ID != "" }

// IsResponse reports whether f answers an earlier request. A response is never
// answered.
func (f Frame) IsResponse() bool { return f.ReplyTo != "" }

// Validate reports whether f is structurally addressable. It rejects only what
// makes a frame unroutable or unsafe to log; it deliberately says nothing
// about whether Type is known, since rejecting unknown types is the
// forward-compatibility failure the protocol is built to avoid.
func (f Frame) Validate() error {
	switch {
	case f.Type == "":
		return ErrEmptyType
	case len(f.Type) > MaxTypeBytes:
		return fmt.Errorf("%w: %d bytes", ErrTypeTooLong, len(f.Type))
	case f.ID != "" && f.ReplyTo != "":
		return ErrBothIDs
	case f.Session != "" && f.Instance == "":
		return ErrSessionAlone
	}
	for name, v := range map[string]string{
		"type": f.Type, "instance": f.Instance, "session": f.Session,
		"id": f.ID, "re": f.ReplyTo,
	} {
		if name != "type" && len(v) > MaxIDBytes {
			return fmt.Errorf("%w: %s is %d bytes", ErrIDTooLong, name, len(v))
		}
		// Control characters cannot break NDJSON framing (JSON escapes
		// them inside strings) but they do corrupt logs and routing
		// keys, and an identifier has no legitimate use for one.
		if i := strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
			return fmt.Errorf("%w: %s at byte %d", ErrControlChar, name, i)
		}
		// Invalid UTF-8 is rejected rather than carried, because JSON
		// encoding silently rewrites it to U+FFFD: a routing key that
		// survives one hop as one value and the next as another is not
		// an identifier. Decoded frames always pass this — the decoder
		// has already done the substitution — so the rule costs nothing
		// on the receive path and only stops a local caller minting an
		// identifier that will not survive being sent.
		if !utf8.ValidString(v) {
			return fmt.Errorf("%w: %s", ErrNotUTF8, name)
		}
	}
	return nil
}

// WithPayload returns f carrying v as its payload, encoded as compact JSON.
// It is the only way to build a payload that is guaranteed newline-free, and
// therefore the only way callers should build one.
func (f Frame) WithPayload(v any) (Frame, error) {
	b, err := marshalCompact(v)
	if err != nil {
		return f, fmt.Errorf("librelay: encode payload for %q: %w", f.Type, err)
	}
	f.Payload = b
	return f, nil
}

// DecodePayload unmarshals f's payload into v. An absent payload decodes to
// nothing and is not an error, so a peer may add a payload to a frame that
// previously had none without breaking older readers.
func (f Frame) DecodePayload(v any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("librelay: decode payload of %q: %w", f.Type, err)
	}
	return nil
}

// Unsupported returns the reply owed to a frame this build cannot handle, and
// whether one is owed at all. It lives here rather than in either endpoint
// because the compatibility rule is only worth anything if both ends implement
// the same one.
//
// A request is answered with [TypeError]/[CodeUnsupportedType] so its sender
// fails immediately instead of waiting out a timeout — a timeout is how a
// version mismatch turns into a hang. A notification or a response is dropped:
// nothing is waiting on it, and answering a response is how two peers deadlock
// each other into an error loop.
func Unsupported(f Frame) (Frame, bool) {
	if !f.IsRequest() {
		return Frame{}, false
	}
	reply := Frame{
		Type:     TypeError,
		Instance: f.Instance,
		Session:  f.Session,
		ReplyTo:  f.ID,
	}
	// Marshaling a struct of strings cannot fail; the error is dropped
	// rather than propagated so callers are not forced to handle an
	// impossible case in their read loop.
	reply, _ = reply.WithPayload(Error{
		Code:    CodeUnsupportedType,
		Message: "unsupported message type " + clampType(f.Type),
	})
	return reply, true
}

// NewError builds a response carrying code and message, correlated to req when
// req is a request. It returns a notification-shaped error frame otherwise,
// which a peer logs rather than routes.
func NewError(req Frame, code, message string) Frame {
	f := Frame{Type: TypeError, Instance: req.Instance, Session: req.Session, ReplyTo: req.ID}
	f, _ = f.WithPayload(Error{Code: code, Message: message})
	return f
}

// clampType bounds an untrusted type before it is echoed into a message, so a
// peer cannot use the error path to make this end emit a frame larger than the
// one it received.
func clampType(t string) string {
	if len(t) > MaxTypeBytes {
		return t[:MaxTypeBytes] + "…"
	}
	return t
}
