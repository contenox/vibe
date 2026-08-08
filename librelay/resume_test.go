package librelay_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/librelay"
)

// Seq must survive the codec, and must be absent from the wire when unset:
// control traffic is unsequenced, and a zero that serialised would make every
// heartbeat look like the first frame of a session.
func TestUnit_Frame_SeqRoundTripsAndOmitsZero(t *testing.T) {
	var buf bytes.Buffer
	w := librelay.NewWriter(&buf)

	sequenced := librelay.Frame{Type: librelay.TypeACPMessage, Instance: "i", Session: "s", Seq: 42}
	if err := w.WriteFrame(sequenced); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"seq":42`)) {
		t.Fatalf("seq missing from the wire: %s", buf.String())
	}

	got, err := librelay.NewReader(bytes.NewReader(buf.Bytes())).ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Seq != 42 {
		t.Fatalf("Seq = %d, want 42", got.Seq)
	}

	buf.Reset()
	if err := w.WriteFrame(librelay.Frame{Type: librelay.TypeHeartbeat, Instance: "i"}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"seq"`)) {
		t.Fatalf("unsequenced frame carried a seq: %s", buf.String())
	}
}

// Resumption is end-to-end, not relay control traffic: a relay must route these
// rather than answer them, or it has to retain session content.
func TestUnit_Resume_IsNotControlTraffic(t *testing.T) {
	for _, typ := range []string{librelay.TypeResume, librelay.TypeResumed} {
		if librelay.IsControl(typ) {
			t.Fatalf("%q is control traffic; a relay would have to answer it", typ)
		}
	}
}

// A request always gets exactly one answer, so a peer that does not implement
// resumption must refuse rather than ignore — otherwise the requester blocks on
// a reply that never comes.
func TestUnit_Resume_UnknownToAnOlderPeerIsAnswered(t *testing.T) {
	req := librelay.Frame{Type: librelay.TypeResume, Instance: "i", Session: "s", ID: "r1"}
	reply, owed := librelay.Unsupported(req)
	if !owed {
		t.Fatal("a resume request must be answerable by a peer that does not know it")
	}
	if reply.ReplyTo != "r1" {
		t.Fatalf("reply correlates to %q, want r1", reply.ReplyTo)
	}
}

func TestUnit_Resume_PayloadsRoundTrip(t *testing.T) {
	f, err := librelay.Frame{Type: librelay.TypeResume, Instance: "i", Session: "s", ID: "r1"}.
		WithPayload(librelay.Resume{AfterSeq: 17})
	if err != nil {
		t.Fatalf("encode resume: %v", err)
	}
	var req librelay.Resume
	if err := f.DecodePayload(&req); err != nil {
		t.Fatalf("decode resume: %v", err)
	}
	if req.AfterSeq != 17 {
		t.Fatalf("AfterSeq = %d, want 17", req.AfterSeq)
	}

	f, err = librelay.Frame{Type: librelay.TypeResumed, Instance: "i", Session: "s", ReplyTo: "r1"}.
		WithPayload(librelay.Resumed{FromSeq: 30, Evicted: true})
	if err != nil {
		t.Fatalf("encode resumed: %v", err)
	}
	var ans librelay.Resumed
	if err := f.DecodePayload(&ans); err != nil {
		t.Fatalf("decode resumed: %v", err)
	}
	if ans.FromSeq != 30 || !ans.Evicted {
		t.Fatalf("Resumed = %+v, want FromSeq 30 evicted", ans)
	}

	// Eviction must be explicit on the wire. Answered without it, a receiver
	// cannot tell a continuous stream from one with a hole in it.
	raw, err := json.Marshal(librelay.Resumed{FromSeq: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("evicted")) {
		t.Fatalf("a continuous resume carried an evicted flag: %s", raw)
	}
}

// The retry hint must survive the handshake payload, since a draining relay
// uses it to stagger the fleet's return.
func TestUnit_Welcome_RetryHintRoundTrips(t *testing.T) {
	f, err := librelay.Frame{Type: librelay.TypeWelcome, Instance: "i", ReplyTo: "h1"}.
		WithPayload(librelay.Welcome{ProtocolVersion: librelay.ProtocolVersion, RetryAfterSeconds: 20})
	if err != nil {
		t.Fatalf("encode welcome: %v", err)
	}
	var w librelay.Welcome
	if err := f.DecodePayload(&w); err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if w.RetryAfterSeconds != 20 {
		t.Fatalf("RetryAfterSeconds = %d, want 20", w.RetryAfterSeconds)
	}
}
