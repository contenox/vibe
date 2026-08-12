// Package relaytest is an in-memory stand-in for a relay, for testing a
// connector without a network. Links are net.Pipe pairs, so nothing here
// binds a socket, and every [Relay] generates a real signing identity so a
// connector's verification path is exercised against it.
package relaytest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/contenox/contenox/libcipher"
	"github.com/contenox/contenox/librelay"
)

const recvBuffer = 256

const defaultDeadline = 10 * time.Second

// Relay is a fake relay implementing hello, heartbeat, and cargo routing by
// (instance, session); the zero value is not usable, call [New].
type Relay struct {
	autoControl bool
	priv        libcipher.SigningPrivateKey
	sign        bool

	mu    sync.Mutex
	links []*Link
	byID  map[string]*Link
}

// Option configures a [Relay].
type Option func(*Relay)

// NoAutoControl stops the relay answering hello and heartbeat, leaving every
// frame to the test.
func NoAutoControl() Option { return func(r *Relay) { r.autoControl = false } }

// SigningKey replaces the generated identity, for a test that must pin a
// specific key or share one identity across two relays.
func SigningKey(priv libcipher.SigningPrivateKey) Option { return func(r *Relay) { r.priv = priv } }

// NoSignature makes the relay answer welcome without signing the connector's
// nonce.
func NoSignature() Option { return func(r *Relay) { r.sign = false } }

// New returns a running fake relay with a fresh signing identity.
func New(opts ...Option) *Relay {
	_, priv, err := libcipher.GenerateSigningKey()
	if err != nil {
		// No error to return, and every caller would ignore one anyway.
		panic("relaytest: generate signing key: " + err.Error())
	}
	r := &Relay{autoControl: true, priv: priv, sign: true, byID: map[string]*Link{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// PublicKey is the relay's identity in the encoding
// [librelay.ParsePublicKey] reads, ready to be handed to a connector as its
// pinned key.
func (r *Relay) PublicKey() string {
	return librelay.FormatPublicKey(r.priv.Public().(libcipher.SigningPublicKey))
}

// Link is one connector's connection; it owns the relay side of a pipe and
// runs a read loop, and the connector under test gets the other side from
// [Link.Conn].
type Link struct {
	relay *Relay
	conn  net.Conn
	srv   net.Conn
	w     *librelay.Writer
	recv  chan librelay.Frame
	out   chan librelay.Frame

	closeOnce sync.Once
	done      chan struct{}

	mu       sync.Mutex
	instance string
	errs     []error
}

// Dial returns a new Link; named for the connector's direction, not an
// accept — nothing here listens.
func (r *Relay) Dial() *Link {
	relaySide, connectorSide := net.Pipe()
	l := &Link{
		relay: r,
		conn:  connectorSide,
		srv:   relaySide,
		w:     librelay.NewWriter(relaySide),
		recv:  make(chan librelay.Frame, recvBuffer),
		out:   make(chan librelay.Frame, recvBuffer),
		done:  make(chan struct{}),
	}
	r.mu.Lock()
	r.links = append(r.links, l)
	r.mu.Unlock()
	go l.read()
	go l.write()
	return l
}

// Conn returns the connector's end of the link; closing it looks like a
// connector-side hangup to the relay.
func (l *Link) Conn() net.Conn { return l.conn }

func (l *Link) read() {
	defer close(l.done)
	defer l.srv.Close()
	rd := librelay.NewReader(l.srv)
	for {
		f, err := rd.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				l.note(err)
			}
			// Only a framing error kills the stream; a malformed frame
			// leaves the reader usable.
			if errors.Is(err, librelay.ErrFrameTooLarge) || errors.Is(err, librelay.ErrReaderClosed) ||
				errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			continue
		}
		l.queue(f)
		if l.relay.autoControl {
			l.autoRespond(f)
		}
	}
}

func (l *Link) autoRespond(f librelay.Frame) {
	if !librelay.IsControl(f.Type) || f.IsResponse() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultDeadline)
	defer cancel()
	switch f.Type {
	case librelay.TypeHello:
		var h librelay.Hello
		if err := f.DecodePayload(&h); err != nil {
			_ = l.Send(ctx, librelay.NewError(f, librelay.CodeMalformedFrame, err.Error()))
			return
		}
		if h.Instance == "" || (f.Instance != "" && h.Instance != f.Instance) {
			// Frame's routing key and the payload's claim must agree, or
			// routing and accounting would use different identities.
			_ = l.Send(ctx, librelay.NewError(f, librelay.CodeUnknownInstance, "hello instance does not match frame instance"))
			return
		}
		l.bind(h.Instance)
		version := min(h.ProtocolVersion, librelay.ProtocolVersion)
		welcome := librelay.Welcome{ProtocolVersion: version, Relay: "relaytest"}
		if l.relay.sign && len(h.Nonce) > 0 {
			// Signed over the version selected, not offered: the connector
			// verifies against what it was told.
			sig, err := librelay.SignWelcome(l.relay.priv, h.Nonce, version, h.Instance)
			if err != nil {
				_ = l.Send(ctx, librelay.NewError(f, librelay.CodeMalformedFrame, err.Error()))
				return
			}
			welcome.Signature = sig
		}
		reply := librelay.Frame{Type: librelay.TypeWelcome, Instance: h.Instance, ReplyTo: f.ID}
		reply, _ = reply.WithPayload(welcome)
		_ = l.Send(ctx, reply)
	case librelay.TypeHeartbeat:
		if !f.IsRequest() {
			return
		}
		_ = l.Send(ctx, librelay.Frame{Type: librelay.TypeAck, Instance: f.Instance, ReplyTo: f.ID})
	case librelay.TypeAck, librelay.TypeWelcome, librelay.TypeError:
		// Responses and relay-authored types: nothing is owed.
	default:
		if reply, owed := librelay.Unsupported(f); owed {
			_ = l.Send(ctx, reply)
		}
	}
}

// Send queues a frame for delivery to the connector, preserving order, and
// returns once queued rather than once written.
func (l *Link) Send(ctx context.Context, f librelay.Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	select {
	case l.out <- f:
		return nil
	case <-l.done:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Link) write() {
	for {
		select {
		case f := <-l.out:
			if err := l.w.WriteFrame(f); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					l.note(err)
				}
				return
			}
		case <-l.done:
			return
		}
	}
}

// Recv returns the next frame the connector sent, in order.
func (l *Link) Recv(ctx context.Context) (librelay.Frame, error) {
	select {
	case f := <-l.recv:
		return f, nil
	case <-l.done:
		// Drains anything queued before the hangup, so a test can still
		// see the last frame sent.
		select {
		case f := <-l.recv:
			return f, nil
		default:
			return librelay.Frame{}, io.EOF
		}
	case <-ctx.Done():
		return librelay.Frame{}, ctx.Err()
	}
}

// Drop slams the connection shut without a close frame, the way a relay
// restart looks from the connector's side; idempotent.
func (l *Link) Drop() {
	// srv, not conn: closing the connector's end would simulate a
	// connector hangup, not a relay outage.
	l.closeOnce.Do(func() { _ = l.srv.Close() })
}

// Instance returns the instance this link bound during hello, or "" if it has
// not identified itself.
func (l *Link) Instance() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.instance
}

// Errors returns the decode and framing errors the relay saw on this link.
func (l *Link) Errors() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// Route delivers f to whichever link bound f.Instance during hello.
func (r *Relay) Route(ctx context.Context, f librelay.Frame) error {
	if f.Instance == "" {
		return fmt.Errorf("relaytest: frame of type %q has no instance to route to", f.Type)
	}
	r.mu.Lock()
	l := r.byID[f.Instance]
	r.mu.Unlock()
	if l == nil {
		return fmt.Errorf("relaytest: no link bound to instance %q", f.Instance)
	}
	return l.Send(ctx, f)
}

// Links returns every link created so far, in creation order.
func (r *Relay) Links() []*Link {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Link(nil), r.links...)
}

// Close drops every link.
func (r *Relay) Close() {
	for _, l := range r.Links() {
		l.Drop()
	}
}

func (l *Link) bind(instance string) {
	l.mu.Lock()
	l.instance = instance
	l.mu.Unlock()
	l.relay.mu.Lock()
	l.relay.byID[instance] = l
	l.relay.mu.Unlock()
}

func (l *Link) note(err error) {
	l.mu.Lock()
	l.errs = append(l.errs, err)
	l.mu.Unlock()
}

func (l *Link) queue(f librelay.Frame) {
	select {
	case l.recv <- f:
	default:
		l.note(fmt.Errorf("relaytest: receive buffer full, dropped frame of type %q", f.Type))
	}
}
