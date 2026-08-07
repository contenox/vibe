package librelay_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/contenox/contenox/librelay"
)

func TestUnit_FrameValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame librelay.Frame
		want  error
	}{
		{"acp cargo", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Session: "s1"}, nil},
		{"control without session", librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "i1", ID: "1"}, nil},
		{"unknown type is valid", librelay.Frame{Type: "future.thing", Instance: "i1"}, nil},
		{"unknown control type is valid", librelay.Frame{Type: "relay.future", Instance: "i1", ID: "1"}, nil},
		{"hello before identification", librelay.Frame{Type: librelay.TypeHello, ID: "1"}, nil},
		{"empty type", librelay.Frame{Instance: "i1"}, librelay.ErrEmptyType},
		{"type too long", librelay.Frame{Type: strings.Repeat("t", librelay.MaxTypeBytes+1)}, librelay.ErrTypeTooLong},
		{"request and response at once", librelay.Frame{Type: librelay.TypeAck, ID: "1", ReplyTo: "2"}, librelay.ErrBothIDs},
		{"session without instance", librelay.Frame{Type: librelay.TypeACPMessage, Session: "s1"}, librelay.ErrSessionAlone},
		{"instance too long", librelay.Frame{Type: librelay.TypeACPMessage, Instance: strings.Repeat("i", librelay.MaxIDBytes+1)}, librelay.ErrIDTooLong},
		{"newline in instance", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i\n1"}, librelay.ErrControlChar},
		{"control char in type", librelay.Frame{Type: "acp.\x00message", Instance: "i1"}, librelay.ErrControlChar},
		{"del in session", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1", Session: "s\x7f"}, librelay.ErrControlChar},
		// JSON encoding would rewrite this to U+FFFD, so the instance a
		// relay routed on would not be the instance the sender named.
		{"instance is not utf-8", librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i\xcb\xcb"}, librelay.ErrNotUTF8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.frame.Validate()
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUnit_FrameRequestResponseDirection(t *testing.T) {
	t.Parallel()
	req := librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "i1", ID: "h7"}
	if !req.IsRequest() || req.IsResponse() {
		t.Fatalf("request classified as %v/%v", req.IsRequest(), req.IsResponse())
	}
	resp := librelay.Frame{Type: librelay.TypeAck, Instance: "i1", ReplyTo: "h7"}
	if resp.IsRequest() || !resp.IsResponse() {
		t.Fatalf("response classified as %v/%v", resp.IsRequest(), resp.IsResponse())
	}
	note := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1"}
	if note.IsRequest() || note.IsResponse() {
		t.Fatalf("notification classified as %v/%v", note.IsRequest(), note.IsResponse())
	}
}

func TestUnit_IsControlSplitsTheTypeSpace(t *testing.T) {
	t.Parallel()
	for _, ty := range []string{librelay.TypeHello, librelay.TypeWelcome, librelay.TypeHeartbeat, librelay.TypeAck, librelay.TypeError, "relay.invented.later"} {
		if !librelay.IsControl(ty) {
			t.Fatalf("IsControl(%q) = false, want true", ty)
		}
	}
	for _, ty := range []string{librelay.TypeACPMessage, "acp.future", "relayish.thing", "", "Relay.hello"} {
		if librelay.IsControl(ty) {
			t.Fatalf("IsControl(%q) = true, want false", ty)
		}
	}
}

// TestUnit_UnsupportedAnswersOnlyRequests pins the compatibility rule: a peer
// that sends something we do not understand must never be left waiting, and a
// peer that receives something it does not understand must never answer an
// answer. The second half is what stops an error loop between two versions.
func TestUnit_UnsupportedAnswersOnlyRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame librelay.Frame
		owed  bool
	}{
		{"unknown control request", librelay.Frame{Type: "relay.from.the.future", Instance: "i1", ID: "42"}, true},
		{"unknown cargo request", librelay.Frame{Type: "future.cargo", Instance: "i1", Session: "s1", ID: "43"}, true},
		{"notification", librelay.Frame{Type: "relay.from.the.future", Instance: "i1"}, false},
		{"response", librelay.Frame{Type: "relay.from.the.future", Instance: "i1", ReplyTo: "42"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reply, owed := librelay.Unsupported(tc.frame)
			if owed != tc.owed {
				t.Fatalf("Unsupported() owed = %v, want %v", owed, tc.owed)
			}
			if !owed {
				return
			}
			if reply.Type != librelay.TypeError {
				t.Fatalf("reply type = %q, want %q", reply.Type, librelay.TypeError)
			}
			if reply.ReplyTo != tc.frame.ID || reply.ID != "" {
				t.Fatalf("reply correlation = id:%q re:%q, want re:%q", reply.ID, reply.ReplyTo, tc.frame.ID)
			}
			if reply.Instance != tc.frame.Instance || reply.Session != tc.frame.Session {
				t.Fatalf("reply routing = (%q,%q), want (%q,%q)", reply.Instance, reply.Session, tc.frame.Instance, tc.frame.Session)
			}
			var e librelay.Error
			if err := reply.DecodePayload(&e); err != nil {
				t.Fatalf("DecodePayload: %v", err)
			}
			if e.Code != librelay.CodeUnsupportedType {
				t.Fatalf("code = %q, want %q", e.Code, librelay.CodeUnsupportedType)
			}
			if err := reply.Validate(); err != nil {
				t.Fatalf("reply is invalid: %v", err)
			}
			// The reply must itself be unanswerable, or two peers
			// that both fail to understand each other never stop.
			if _, again := librelay.Unsupported(reply); again {
				t.Fatal("Unsupported() answered its own reply")
			}
		})
	}
}

// TestUnit_UnsupportedClampsEchoedType asserts an oversized type cannot be
// amplified back out through the error path.
func TestUnit_UnsupportedClampsEchoedType(t *testing.T) {
	t.Parallel()
	huge := "relay." + strings.Repeat("x", 8192)
	reply, owed := librelay.Unsupported(librelay.Frame{Type: huge, Instance: "i1", ID: "1"})
	if !owed {
		t.Fatal("no reply owed for a request")
	}
	if len(reply.Payload) > 2*librelay.MaxTypeBytes {
		t.Fatalf("reply payload is %d bytes, want it clamped near MaxTypeBytes", len(reply.Payload))
	}
}

// TestUnit_UnknownFieldsSurviveDecoding is the field-level half of the
// compatibility rule: an older build must decode a frame a newer build wrote.
func TestUnit_UnknownFieldsSurviveDecoding(t *testing.T) {
	t.Parallel()
	line := `{"type":"acp.message","instance":"i1","session":"s1","payload":{"a":1},"invented_later":{"deep":[1,2]},"v":7}`
	f, err := librelay.NewReader(strings.NewReader(line + "\n")).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if f.Instance != "i1" || f.Session != "s1" || f.Type != librelay.TypeACPMessage {
		t.Fatalf("frame = %+v", f)
	}
	if string(f.Payload) != `{"a":1}` {
		t.Fatalf("payload = %s", f.Payload)
	}
}

func TestUnit_WithPayloadCompactsAndPreservesBytes(t *testing.T) {
	t.Parallel()
	pretty := json.RawMessage("{\n  \"jsonrpc\": \"2.0\",\n  \"method\": \"session/update\"\n}")
	f, err := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i1"}.WithPayload(pretty)
	if err != nil {
		t.Fatalf("WithPayload: %v", err)
	}
	if strings.ContainsAny(string(f.Payload), "\n\r") {
		t.Fatalf("payload retained a newline: %q", f.Payload)
	}
	if got, want := string(f.Payload), `{"jsonrpc":"2.0","method":"session/update"}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestUnit_DecodePayloadTolerantOfAbsence(t *testing.T) {
	t.Parallel()
	var h librelay.Hello
	if err := (librelay.Frame{Type: librelay.TypeHeartbeat}).DecodePayload(&h); err != nil {
		t.Fatalf("DecodePayload on an empty payload: %v", err)
	}
	// reflect rather than ==: Hello carries a nonce, and a struct with a
	// slice in it is not comparable.
	if !reflect.DeepEqual(h, librelay.Hello{}) {
		t.Fatalf("payload decoded to %+v, want zero", h)
	}
}
