// Package relayacp carries ACP over a relay connection, so a remote client is
// just another ACP client of this runtime, routed by [librelay.Frame.Session] to
// its own [libacp.AgentSideConnection].
package relayacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/librelay"
	"github.com/contenox/contenox/libtracker"
)

// Bounds [Config] leaves zero.
const (
	// DefaultQueue is how many inbound messages may await one attachment's
	// ACP connection before it is judged wedged.
	DefaultQueue = 64
	// DefaultMaxAttachments bounds the attachments one tunnel serves at once.
	DefaultMaxAttachments = 64
)

// SendFunc puts one frame on the relay; it must not block.
type SendFunc func(librelay.Frame) error

// Config is everything a tunnel needs.
type Config struct {
	// Instance is this runtime's identity at the relay, stamped on every
	// outbound frame; required.
	Instance string
	// Send is the outbound edge; required.
	Send SendFunc
	// Factory builds the agent serving one attachment; required, and must be
	// safe for concurrent use.
	Factory libacp.AgentFactory
	// Queue is the per-attachment inbound depth; zero means [DefaultQueue].
	Queue int
	// MaxAttachments caps concurrent attachments; zero means
	// [DefaultMaxAttachments].
	MaxAttachments int
	// Tracker instruments each attachment's lifetime; nil means
	// [libtracker.NoopTracker].
	Tracker libtracker.ActivityTracker
}

// Tunnel routes relay cargo to per-attachment ACP connections. Use [New]; it is
// safe for concurrent use.
type Tunnel struct {
	cfg     Config
	tracker libtracker.ActivityTracker

	ctx    context.Context
	cancel context.CancelFunc

	clock atomic.Uint64

	wg sync.WaitGroup

	mu     sync.Mutex
	live   map[string]*attachment
	closed bool
}

// New validates cfg and returns a tunnel that has attached nothing.
func New(cfg Config) (*Tunnel, error) {
	if cfg.Instance == "" {
		return nil, errors.New("relayacp: Instance is required")
	}
	if cfg.Send == nil {
		return nil, errors.New("relayacp: Send is required")
	}
	if cfg.Factory == nil {
		return nil, errors.New("relayacp: Factory is required")
	}
	probe := librelay.Frame{Type: librelay.TypeACPMessage, Instance: cfg.Instance, Session: "probe"}
	if err := probe.Validate(); err != nil {
		return nil, fmt.Errorf("relayacp: Instance: %w", err)
	}
	if cfg.Queue <= 0 {
		cfg.Queue = DefaultQueue
	}
	if cfg.MaxAttachments <= 0 {
		cfg.MaxAttachments = DefaultMaxAttachments
	}
	if cfg.Tracker == nil {
		cfg.Tracker = libtracker.NoopTracker{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Tunnel{
		cfg:     cfg,
		tracker: cfg.Tracker,
		ctx:     ctx,
		cancel:  cancel,
		live:    map[string]*attachment{},
	}, nil
}

// Handle applies one routed frame to the attachment named by
// [librelay.Frame.Session]. It never blocks or fails, dropping what it cannot
// place.
func (t *Tunnel) Handle(ctx context.Context, f librelay.Frame) {
	if f.Session == "" {
		return
	}
	if f.Instance != "" && f.Instance != t.cfg.Instance {
		return
	}
	switch f.Type {
	case librelay.TypeACPMessage:
		if len(f.Payload) == 0 {
			return
		}
		if a := t.attachmentFor(ctx, f.Session); a != nil {
			a.deliver(f.Payload)
		}
	case librelay.TypeACPDetach:
		t.detachSession(f.Session)
	}
}

// Len reports how many attachments are live; diagnostics only.
func (t *Tunnel) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// Close drops every attachment and returns once each one's ACP connection and
// goroutine has exited. It is idempotent.
func (t *Tunnel) Close() {
	t.mu.Lock()
	first := !t.closed
	t.closed = true
	live := make([]*attachment, 0, len(t.live))
	for _, a := range t.live {
		live = append(live, a)
	}
	t.mu.Unlock()
	if first {
		t.cancel()
	}
	// Closed after the lock is released to avoid deadlocking against an
	// attachment deregistering itself.
	for _, a := range live {
		a.close()
	}
	t.wg.Wait()
}

func (t *Tunnel) detachSession(session string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.live[session]
	if a == nil {
		return
	}
	delete(t.live, session)
	a.close()
}

func (t *Tunnel) attachmentFor(ctx context.Context, session string) *attachment {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if a := t.live[session]; a != nil {
		a.touch(t.clock.Add(1))
		return a
	}
	// At the cap, evict inline: eviction only closes a channel.
	for len(t.live) >= t.cfg.MaxAttachments {
		oldest := t.oldestLocked()
		if oldest == nil {
			return nil
		}
		delete(t.live, oldest.session)
		oldest.close()
	}
	a := newAttachment(session, t.cfg.Instance, t.cfg.Queue, t.cfg.Send)
	a.touch(t.clock.Add(1))
	t.live[session] = a
	t.wg.Add(1)
	go t.run(a, libtracker.CopyTrackingValues(ctx, t.ctx))
	return a
}

func (t *Tunnel) oldestLocked() *attachment {
	var oldest *attachment
	for _, a := range t.live {
		if oldest == nil || a.seen() < oldest.seen() {
			oldest = a
		}
	}
	return oldest
}

func (t *Tunnel) run(a *attachment, openCtx context.Context) {
	defer t.wg.Done()
	defer t.detach(a)
	reportErr, _, end := t.tracker.Start(openCtx, "hold", "relay_attachment", "session", a.session)
	defer end()

	// The connection runs under t.ctx, not openCtx: an attachment outlives the
	// action that opened it.
	conn := libacp.NewAgentSideConnection(a.stream, t.cfg.Factory)
	err := conn.Run(t.ctx)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, io.ErrClosedPipe) {
		reportErr(err)
	}
	a.close()
}

func (t *Tunnel) detach(a *attachment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Identity-checked so an eviction that replaced this session is not undone.
	if t.live[a.session] == a {
		delete(t.live, a.session)
	}
}

type attachment struct {
	session  string
	stream   *stream
	lastSeen atomic.Uint64
}

func newAttachment(session, instance string, queue int, send SendFunc) *attachment {
	return &attachment{
		session: session,
		stream:  newStream(session, instance, queue, send),
	}
}

func (a *attachment) touch(at uint64) { a.lastSeen.Store(at) }

func (a *attachment) seen() uint64 { return a.lastSeen.Load() }

func (a *attachment) close() { a.stream.Close() }

func (a *attachment) deliver(payload json.RawMessage) {
	if !a.stream.offer(payload) {
		a.close()
	}
}
