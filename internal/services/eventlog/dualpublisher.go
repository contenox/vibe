package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
)

// HopEnvVar carries the dispatch hop across a process boundary. In-process the
// hop rides the execution context (runtimetypes.WithEventHop), which dies at
// the exec boundary — and the exec boundary is the trigger tier's primary
// actuation mode: a fired chain running `contenox …` through local_shell, or
// spawning this binary's own ACP server for a chain-kind unit, starts a process
// whose appends would otherwise all carry hop 0, so a trigger listening on the
// events its own chain causes would never spend DefaultMaxHop. A spawn site
// stamps this variable with the hop its own context carries (the value is
// passed verbatim, not incremented: the child is that chain's actuation, not a
// further generation); NewDualPublisher reads it once, at construction.
const HopEnvVar = "CONTENOX_EVENT_HOP"

// Publisher is the narrow publish seam dual-writing producers already hold
// (missionservice.EventPublisher is identical); libbus.Messenger satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// DualPublisher upgrades an existing bus publisher to dual-write: Publish
// appends (subject, data) to the durable log as an event of type subject,
// stamped with the producer's workspace, then forwards to the wrapped
// publisher verbatim — store first, live copy second, existing subscribers
// see exactly the payload they always did (subjects stay unscoped). A failed
// append is reported and never blocks the live publish.
type DualPublisher struct {
	log          runtimetypes.EventStore
	next         Publisher
	source       string
	workspaceID  string
	subjectField string
	tracker      libtracker.ActivityTracker
	trigger      Trigger
	// inheritedHop is the hop this process was spawned with (HopEnvVar), read
	// once at construction. Zero when unspawned.
	inheritedHop int
}

// DualPublisherOption configures a DualPublisher at construction.
type DualPublisherOption func(*DualPublisher)

// WithSubjectField names a top-level string field of the payload to record as
// the stored event's Subject (e.g. "missionId"). Best-effort: a payload
// without the field stores an empty subject.
func WithSubjectField(field string) DualPublisherOption {
	return func(p *DualPublisher) { p.subjectField = field }
}

// WithPublisherTrigger installs the in-process trigger hook on the dual-write
// path — the same seam WithTrigger gives the service: every successfully
// appended event is handed to t after the live publish, asynchronously.
func WithPublisherTrigger(t Trigger) DualPublisherOption {
	return func(p *DualPublisher) {
		if t != nil {
			p.trigger = t
		}
	}
}

// NewDualPublisher wraps next with a durable append to log. source and the
// producer's workspaceID (the runtime's ResolveWorkspaceID result at the
// wiring site) are stamped on every appended event; a nil tracker degrades
// to Noop.
func NewDualPublisher(log runtimetypes.EventStore, next Publisher, source, workspaceID string, tracker libtracker.ActivityTracker, opts ...DualPublisherOption) *DualPublisher {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	p := &DualPublisher{log: log, next: next, source: source, workspaceID: workspaceID, tracker: tracker, inheritedHop: hopFromEnv()}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// SpawnEnv returns the environment entries a child process needs to keep the
// event tier's invariants across the exec boundary — today only the dispatch
// hop, when ctx carries one. Nil otherwise, so a spawn site can merge it
// unconditionally. Spawn sites outside this package (the local_shell runner
// above all: a fired chain's primary actuation mode) call it instead of
// spelling HopEnvVar themselves.
func SpawnEnv(ctx context.Context) map[string]string {
	hop := runtimetypes.EventHopFromContext(ctx)
	if hop <= 0 {
		return nil
	}
	return map[string]string{HopEnvVar: strconv.Itoa(hop)}
}

// InheritHop returns ctx carrying the hop this process was spawned with
// (HopEnvVar), unchanged when ctx already carries one — SpawnEnv's read side,
// for a host seeding its root context. NewDualPublisher covers the append it
// owns without this, but only this puts the hop back on a context, which is
// what the rest of the tier reads: a spawn made by this process (SpawnEnv,
// agentinstance's ChainHopEnvVar) can only forward a hop its context carries.
func InheritHop(ctx context.Context) context.Context {
	if runtimetypes.EventHopFromContext(ctx) > 0 {
		return ctx
	}
	return runtimetypes.WithEventHop(ctx, hopFromEnv())
}

// hopFromEnv reads HopEnvVar. A missing, malformed, or non-positive value is
// hop 0 — the same answer an unspawned process gives — never an error: a broken
// variable must not stop a producer from publishing.
func hopFromEnv() int {
	hop, err := strconv.Atoi(strings.TrimSpace(os.Getenv(HopEnvVar)))
	if err != nil || hop <= 0 {
		return 0
	}
	return hop
}

func (p *DualPublisher) Publish(ctx context.Context, subject string, data []byte) error {
	event := &runtimetypes.Event{
		WorkspaceID: p.workspaceID,
		Type:        subject,
		Source:      p.source,
		Subject:     p.extractSubject(data),
		Data:        json.RawMessage(data),
	}
	// The spawner's hop applies only where the appending context carries none:
	// an in-process dispatch's own hop is always the more specific answer, and
	// AppendEvent reads it off the context for a zero-hop event.
	if runtimetypes.EventHopFromContext(ctx) == 0 {
		event.Hop = p.inheritedHop
	}
	if err := p.log.AppendEvent(ctx, event); err != nil {
		reportErr, _, end := p.tracker.Start(ctx, "append", "event_log", "type", subject)
		reportErr(fmt.Errorf("eventlog: dual-write append failed; live publish proceeds: %w", err))
		end()
	} else {
		// The row is durable (NID assigned): fire matching triggers in this
		// process, after the live publish below, never on its error path.
		defer fireTrigger(ctx, p.trigger, event)
	}
	if p.next == nil {
		return nil
	}
	return p.next.Publish(ctx, subject, data)
}

// extractSubject reads the configured subject field from a JSON payload.
func (p *DualPublisher) extractSubject(data []byte) string {
	if p.subjectField == "" {
		return ""
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	var subject string
	if raw, ok := probe[p.subjectField]; ok {
		_ = json.Unmarshal(raw, &subject)
	}
	return subject
}

var _ Publisher = (*DualPublisher)(nil)
