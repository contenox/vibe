package relayacp_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/contenox/contenox/internal/relayacp"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

type traceRecorder struct {
	mu      sync.Mutex
	started []recordedStart
}

type recordedStart struct {
	subject string
	traceID string
	present bool
}

func (r *traceRecorder) Start(ctx context.Context, operation, subject string, _ ...any) (func(error), func(string, any), func()) {
	id, ok := ctx.Value(libtracker.ContextKeyTraceID).(string)
	r.mu.Lock()
	r.started = append(r.started, recordedStart{subject: subject, traceID: id, present: ok})
	r.mu.Unlock()
	return func(error) {}, func(string, any) {}, func() {}
}

func (r *traceRecorder) forSubject(subject string) []recordedStart {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedStart
	for _, s := range r.started {
		if s.subject == subject {
			out = append(out, s)
		}
	}
	return out
}

// TestUnit_AttachmentRecordCarriesTheOpeningTrace checks an attachment's
// lifetime record carries the trace of the frame that created it, not a
// later frame's, and stays untraced when the opening frame was.
func TestUnit_AttachmentRecordCarriesTheOpeningTrace(t *testing.T) {
	t.Parallel()
	var counter atomic.Int64
	rec := &traceRecorder{}
	h := newHarness(t, identityFactory(&counter), func(c *relayacp.Config) { c.Tracker = rec })
	defer h.close()

	opening := librelay.NewTraceID()
	later := librelay.NewTraceID()
	body := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"_x/y"}`)

	h.tunnel.Handle(traced(opening), librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: testInstance, Session: "att-traced", Trace: opening, Payload: body,
	})
	waitFor(t, "the attachment to be created", func() bool { return h.tunnel.Len() == 1 })

	h.tunnel.Handle(traced(later), librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: testInstance, Session: "att-traced", Trace: later, Payload: body,
	})

	h.tunnel.Handle(context.Background(), librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: testInstance, Session: "att-plain", Payload: body,
	})
	waitFor(t, "the untraced attachment to be created", func() bool { return h.tunnel.Len() == 2 })
	waitFor(t, "both attachment records to open", func() bool {
		return len(rec.forSubject("relay_attachment")) == 2
	})

	var withOpening, withoutTrace int
	for _, s := range rec.forSubject("relay_attachment") {
		switch {
		case s.present && s.traceID == opening:
			withOpening++
		case !s.present:
			withoutTrace++
		case s.traceID == later:
			t.Fatal("an attachment record took a later frame's trace; the key must name the action that opened it")
		default:
			t.Fatalf("attachment record carries an unexpected trace %q", s.traceID)
		}
	}
	if withOpening != 1 {
		t.Fatalf("%d records carry the opening frame's trace %q, want 1", withOpening, opening)
	}
	if withoutTrace != 1 {
		t.Fatalf("%d records are untraced, want 1: absent must stay absent", withoutTrace)
	}

	h.tunnel.Handle(traced(later), librelay.Frame{
		Type: librelay.TypeACPMessage, Instance: testInstance, Session: "att-traced", Trace: later, Payload: body,
	})
	if n := len(rec.forSubject("relay_attachment")); n != 2 {
		t.Fatalf("a frame on a live attachment opened a record: %d, want 2", n)
	}
}

func traced(id string) context.Context {
	return context.WithValue(context.Background(), libtracker.ContextKeyTraceID, id)
}
