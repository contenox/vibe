package librelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ProtocolVersion is the envelope version this build speaks; exchanged once in Hello/Welcome, never on a frame.
const ProtocolVersion = 1

// Envelope limits bound what a decoder will allocate for a single frame before it gives up.
const (
	MaxFrameBytes = 16 << 20
	MaxTypeBytes  = 128
	MaxIDBytes    = 256
)

// ControlPrefix marks a type as relay-level control traffic; a relay handles frames with this prefix and forwards everything else.
const ControlPrefix = "relay."

// Control message types; part of the protocol floor and may never be removed.
const (
	// TypeHello is the connector's opening frame, payload [Hello]; a request answered with TypeWelcome or TypeError.
	TypeHello = ControlPrefix + "hello"
	// TypeWelcome is the relay's answer to TypeHello, payload [Welcome].
	TypeWelcome = ControlPrefix + "welcome"
	// TypeHeartbeat is a liveness probe with no payload, sent by either end; a request answered with TypeAck.
	TypeHeartbeat = ControlPrefix + "heartbeat"
	// TypeAck answers TypeHeartbeat, carrying no payload.
	TypeAck = ControlPrefix + "ack"
	// TypeError reports a failure, payload [Error]; as a response it never induces a response.
	TypeError = ControlPrefix + "error"
)

// TypeACPMessage tunnels one ACP JSON-RPC message, byte for byte; a relay routes it without parsing or linking libacp.
const TypeACPMessage = "acp.message"

// TypeACPDetach reports that the client behind one attachment is gone; it carries no payload, and is a hint rather than a prerequisite for anything.
const TypeACPDetach = "acp.detach"

// Chain-trigger types are cargo, not control traffic: a relay addresses a trigger to one instance and the machine answers with the result type.
const (
	// TypeChainTrigger asks the machine behind [Frame.Instance] to run a named task chain, payload [ChainTrigger]; relay→machine only.
	TypeChainTrigger = "chain_trigger"
	// TypeChainTriggerResult reports a chain trigger's outcome, payload [ChainTriggerResult]; machine→relay only, exactly one per [ChainTrigger.RequestID].
	TypeChainTriggerResult = "chain_trigger_result"
)

// [ChainTrigger.SessionMode] values.
const (
	// ChainSessionNew runs the chain in a fresh session; the mode every machine implements.
	ChainSessionNew = "new"
	// ChainSessionReused asks the machine to reuse a session across triggers; unsupported machines refuse rather than silently downgrading to new.
	ChainSessionReused = "reused"
)

// [ChainTriggerResult.Status] values.
const (
	// ChainTriggerStatusOK: the chain ran to completion.
	ChainTriggerStatusOK = "ok"
	// ChainTriggerStatusError: the chain started and failed.
	ChainTriggerStatusError = "error"
	// ChainTriggerStatusRefused: the machine declined before any chain ran; a clean answer, not a run failure.
	ChainTriggerStatusRefused = "refused"
)

// ChainTrigger is the [TypeChainTrigger] payload: run this chain with this input.
type ChainTrigger struct {
	// RequestID correlates the [ChainTriggerResult] with this trigger; minted by the relay, echoed by the machine verbatim.
	RequestID string `json:"request_id"`
	// Chain names the chain file on the machine's own configuration path; what it resolves to is the machine's decision.
	Chain string `json:"chain"`
	// SessionMode is [ChainSessionNew] or [ChainSessionReused].
	SessionMode string `json:"session_mode"`
	// Input is the event envelope the chain receives, raw; the relay never parses it.
	Input json.RawMessage `json:"input"`
	// Policy optionally names the HITL policy envelope for the run; empty applies the machine's own default.
	Policy string `json:"policy,omitempty"`
}

// ChainTriggerResult is the [TypeChainTriggerResult] payload.
type ChainTriggerResult struct {
	// RequestID is [ChainTrigger.RequestID], echoed verbatim.
	RequestID string `json:"request_id"`
	// Status is one of the ChainTriggerStatus constants.
	Status string `json:"status"`
	// Error says why, for [ChainTriggerStatusError] and [ChainTriggerStatusRefused]; empty on ok.
	Error string `json:"error,omitempty"`
}

// Resumption types are not control traffic: a relay routes them to the producer holding the content to replay. See [Resume].
const (
	// TypeResume asks a session's producer to continue after a cursor; always a request, answered exactly once.
	TypeResume = "session.resume"
	// TypeResumed answers [TypeResume] and precedes the replayed frames.
	TypeResumed = "session.resumed"
)

// Error codes carried by [Error]; strings rather than integers so an unrecognized code degrades to something legible in a log.
const (
	CodeUnsupportedType = "unsupported_type"
	CodeMalformedFrame  = "malformed_frame"
	CodeUnknownInstance = "unknown_instance"
	CodeUnauthorized    = "unauthorized"
	CodeVersion         = "unsupported_version"
	// CodeCursorEvicted answers a [Resume] whose cursor the producer no longer retains at all; a partial replay sets [Resumed].Evicted instead.
	CodeCursorEvicted = "cursor_evicted"
)

// Frame is the transport envelope; wire field names are short because every ACP message pays for them. Instance and Session form the routing key and are direction-independent.
type Frame struct {
	// Type discriminates the frame; control types carry ControlPrefix, everything else is routed opaquely.
	Type string `json:"type"`
	// Instance is the runtime instance this frame concerns; empty only on frames that precede identification.
	Instance string `json:"instance,omitempty"`
	// Session names the stream within Instance that this frame concerns; a relay routes on the value without interpreting it. Empty on control traffic and anything instance-scoped.
	Session string `json:"session,omitempty"`
	// ID marks this frame as a request and correlates the reply; its presence, not the type, obliges exactly one answer.
	ID string `json:"id,omitempty"`
	// ReplyTo carries the ID of the request being answered and marks this frame as a response.
	ReplyTo string `json:"re,omitempty"`
	// Seq is the producer's per-(Instance, Session) cursor for this frame, monotonically increasing and gap-free within a session; zero means unsequenced.
	Seq uint64 `json:"seq,omitempty"`
	// Trace is the correlation key for the one human action this frame belongs to; peer-supplied text that must never be authorized on, only logged and joined. [MaxTraceBytes]/[TraceAlphabet] bound it; empty means untraced.
	Trace string `json:"trace,omitempty"`
	// Payload is the message body, left as raw JSON so intermediaries do not parse it; empty is legal.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hello is the [TypeHello] payload: what the connector claims to be, before the relay has agreed to any of it.
type Hello struct {
	// ProtocolVersion is the highest envelope version the connector speaks.
	ProtocolVersion int `json:"protocol_version"`
	// Instance identifies the runtime; also set on the frame since routing must not require reading a payload.
	Instance string `json:"instance"`
	// Agent names the implementation and version, for operator diagnosis only; nothing may branch on it.
	Agent string `json:"agent,omitempty"`
	// Nonce is a fresh random challenge the relay signs into [Welcome.Signature]; generated per connection, never a secret, useful once.
	Nonce []byte `json:"nonce,omitempty"`
}

// Welcome is the [TypeWelcome] payload: the relay's acceptance.
type Welcome struct {
	// ProtocolVersion is the version the relay selected, min(peer, self); the connector must close if it's lower than it can speak.
	ProtocolVersion int `json:"protocol_version"`
	// Relay names the relay implementation and version, diagnostics only.
	Relay string `json:"relay,omitempty"`
	// Signature is Ed25519 over [SigningInput] of Hello.Nonce, ProtocolVersion and instance; omitempty since only a connector pinning a key requires it.
	Signature []byte `json:"sig,omitempty"`
	// RetryAfterSeconds hints how long to wait before redialling; a connector clamps it to its own bounds. Zero means the connector's own backoff applies.
	RetryAfterSeconds int `json:"retry_after,omitempty"`
}

// Resume is the [TypeResume] payload: continue a session's stream after a cursor; not relay control traffic, routed end to end like cargo.
type Resume struct {
	// AfterSeq is the last [Frame.Seq] the requester saw; zero asks for the whole retained stream.
	AfterSeq uint64 `json:"after_seq"`
}

// Resumed is the [TypeResumed] payload: what the producer is about to send.
type Resumed struct {
	// FromSeq is the first [Frame.Seq] that follows; greater than Resume.AfterSeq+1 only when Evicted is set.
	FromSeq uint64 `json:"from_seq"`
	// Evicted reports the requested cursor is older than what the producer retains; a receiver seeing this must refetch state rather than append.
	Evicted bool `json:"evicted,omitempty"`
}

// Error is the [TypeError] payload.
type Error struct {
	// Code is machine-readable and stable; see the Code* constants.
	Code string `json:"code"`
	// Message is for humans and may change freely between versions.
	Message string `json:"message,omitempty"`
}

// Validation failures: all "this frame is not addressable", treated as a bad message rather than a bad connection.
var (
	ErrEmptyType    = errors.New("librelay: frame type is empty")
	ErrTypeTooLong  = errors.New("librelay: frame type exceeds MaxTypeBytes")
	ErrIDTooLong    = errors.New("librelay: identifier exceeds MaxIDBytes")
	ErrControlChar  = errors.New("librelay: identifier contains a control character")
	ErrBothIDs      = errors.New("librelay: frame sets both id and re")
	ErrSessionAlone = errors.New("librelay: frame sets session without instance")
	ErrNotUTF8      = errors.New("librelay: identifier is not valid UTF-8")
)

// IsControl reports whether a type is relay-level control traffic; a relay handles these and forwards everything else.
func IsControl(msgType string) bool { return strings.HasPrefix(msgType, ControlPrefix) }

// IsRequest reports whether f obliges the receiver to send exactly one response.
func (f Frame) IsRequest() bool { return f.ID != "" }

// IsResponse reports whether f answers an earlier request; a response is never answered.
func (f Frame) IsResponse() bool { return f.ReplyTo != "" }

// Validate reports whether f is structurally addressable; it rejects only what makes a frame unroutable or unsafe to log, never an unknown Type.
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
	if err := validateTrace(f.Trace); err != nil {
		return err
	}
	for name, v := range map[string]string{
		"type": f.Type, "instance": f.Instance, "session": f.Session,
		"id": f.ID, "re": f.ReplyTo,
	} {
		if name != "type" && len(v) > MaxIDBytes {
			return fmt.Errorf("%w: %s is %d bytes", ErrIDTooLong, name, len(v))
		}
		// Control characters cannot break NDJSON framing (JSON escapes them inside strings) but they do corrupt logs and routing keys.
		if i := strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
			return fmt.Errorf("%w: %s at byte %d", ErrControlChar, name, i)
		}
		// Invalid UTF-8 is rejected rather than carried: JSON encoding silently rewrites it to U+FFFD, so a routing key would not survive a hop intact.
		if !utf8.ValidString(v) {
			return fmt.Errorf("%w: %s", ErrNotUTF8, name)
		}
	}
	return nil
}

// WithPayload returns f carrying v as its payload, encoded as compact JSON; the only way to guarantee a newline-free payload.
func (f Frame) WithPayload(v any) (Frame, error) {
	b, err := marshalCompact(v)
	if err != nil {
		return f, fmt.Errorf("librelay: encode payload for %q: %w", f.Type, err)
	}
	f.Payload = b
	return f, nil
}

// DecodePayload unmarshals f's payload into v; an absent payload decodes to nothing and is not an error.
func (f Frame) DecodePayload(v any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return fmt.Errorf("librelay: decode payload of %q: %w", f.Type, err)
	}
	return nil
}

// Unsupported returns the reply owed to a frame this build cannot handle, and whether one is owed at all.
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
	// Marshaling a struct of strings cannot fail; the error is dropped rather than forcing callers to handle an impossible case.
	reply, _ = reply.WithPayload(Error{
		Code:    CodeUnsupportedType,
		Message: "unsupported message type " + clampType(f.Type),
	})
	return reply, true
}

// NewError builds a response carrying code and message, correlated to req when req is a request.
func NewError(req Frame, code, message string) Frame {
	f := Frame{Type: TypeError, Instance: req.Instance, Session: req.Session, ReplyTo: req.ID}
	f, _ = f.WithPayload(Error{Code: code, Message: message})
	return f
}

func clampType(t string) string {
	if len(t) > MaxTypeBytes {
		return t[:MaxTypeBytes] + "…"
	}
	return t
}
