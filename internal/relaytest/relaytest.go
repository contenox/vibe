// Package relaytest is an in-memory stand-in for a relay, for testing a
// connector without a network. It is a test double, not a product: it lives
// under internal/ so it can never become something a real relay links against,
// and it is deliberately the only relay-shaped thing in this repository.
//
// It cannot listen. Links are net.Pipe pairs, so there is no socket, no port
// and no address anywhere in this package — a connector under test dials
// nothing and the "never open a listening socket" rule is enforced by there
// being no API that could break it.
//
// It holds an identity. Every [Relay] generates an Ed25519 key and signs the
// handshake with it, so a connector's verification path is exercised here
// rather than only against a deployed relay — which is the difference between
// a check that is tested and a check that is hoped for. Two relays are two
// identities, so "signed by the wrong relay" is a second [New] and needs no
// special mode; [NoSignature] covers the relay that signs nothing at all.
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

// recvBuffer bounds the frames a Link holds for a test that has not read them.
// A test that overruns it is asserting on a firehose and should read as it
// goes; overflow is recorded rather than blocking the read loop, because a
// blocked read loop stops answering heartbeats and turns a test bug into a
// timeout somewhere unrelated.
const recvBuffer = 256

// defaultDeadline bounds a Send whose caller passed a context with no deadline.
// net.Pipe is synchronous, so a Send nobody reads would otherwise hang the test
// binary rather than fail it.
const defaultDeadline = 10 * time.Second

// Relay is a fake relay. The zero value is not usable; call [New].
//
// It implements exactly the relay behavior the envelope was designed to
// require and nothing else: it registers an instance from [librelay.Hello],
// answers heartbeats, applies [librelay.Unsupported] to control types it does
// not know, and treats every other frame as opaque cargo it routes by
// (instance, session) without decoding the payload.
type Relay struct {
	autoControl bool
	// priv is the identity a real relay proves itself with. Every fake
	// relay has one, generated per [New], so two relays in one test are
	// two identities and a test for "signed by the wrong relay" is a
	// second New rather than a special mode.
	priv libcipher.SigningPrivateKey
	sign bool

	mu    sync.Mutex
	links []*Link
	byID  map[string]*Link
}

// Option configures a [Relay].
type Option func(*Relay)

// NoAutoControl stops the relay answering hello and heartbeat, leaving every
// frame to the test. Use it to assert what a connector does when a relay goes
// silent without dropping the connection — the failure mode a plain disconnect
// test misses.
func NoAutoControl() Option { return func(r *Relay) { r.autoControl = false } }

// SigningKey replaces the generated identity, for a test that must pin a
// specific key or share one identity across two relays.
func SigningKey(priv libcipher.SigningPrivateKey) Option { return func(r *Relay) { r.priv = priv } }

// NoSignature makes the relay answer welcome without signing the connector's
// nonce, the way a relay that has not been given a key would. A connector that
// pinned a key must refuse it; this is the case a wrong-key test does not
// cover, because "absent" and "wrong" reach a verifier by different paths.
func NoSignature() Option { return func(r *Relay) { r.sign = false } }

// New returns a running fake relay with a fresh signing identity.
func New(opts ...Option) *Relay {
	_, priv, err := libcipher.GenerateSigningKey()
	if err != nil {
		// crypto/rand failing is not a condition a test double can
		// carry on through, and New has no error to return: every
		// caller would ignore it.
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

// Link is one connector's connection. It owns the relay side of a pipe and
// runs a read loop; the connector under test gets the other side from
// [Link.Conn].
type Link struct {
	relay *Relay
	// conn is the connector's end, handed out by Conn. srv is the relay's
	// end: the one this package reads, writes and closes. Keeping both
	// named matters because Drop must shut the relay's end — closing the
	// connector's end would simulate the connector hanging up, which is
	// the opposite of the failure being tested.
	conn net.Conn
	srv  net.Conn
	w    *librelay.Writer
	recv chan librelay.Frame
	out  chan librelay.Frame

	closeOnce sync.Once
	done      chan struct{}

	mu       sync.Mutex
	instance string
	errs     []error
}

// Dial returns a new Link. The name mirrors the connector's direction — the
// runtime dials the relay — and is not an accept: nothing here is listening
// for it to accept.
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

// Conn returns the connector's end of the link. The connector treats it as an
// already-dialed connection; closing it is what a connector-side hangup looks
// like to the relay.
func (l *Link) Conn() net.Conn { return l.conn }

// read is the relay's read loop. It answers what a relay must answer and
// queues everything else, so a test sees the traffic a real relay would have
// forwarded rather than the traffic it consumed.
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
			// Only a framing error kills the stream; a bad frame
			// leaves the reader usable, and dropping the
			// connection for one would hide the very tolerance
			// this package exists to test.
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

// autoRespond implements the receiver half of the compatibility rule: known
// control types get their defined answer, unknown control types go through
// [librelay.Unsupported], and non-control frames are cargo the relay routes
// without opening.
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
			// The frame's routing key and the payload's claim must
			// agree, or the relay would route by one identity and
			// account by another.
			_ = l.Send(ctx, librelay.NewError(f, librelay.CodeUnknownInstance, "hello instance does not match frame instance"))
			return
		}
		l.bind(h.Instance)
		version := min(h.ProtocolVersion, librelay.ProtocolVersion)
		welcome := librelay.Welcome{ProtocolVersion: version, Relay: "relaytest"}
		if l.relay.sign && len(h.Nonce) > 0 {
			// Signed over the version selected above, not the one
			// offered: the connector verifies against what it was
			// told, so signing the offer would never verify.
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

// Send queues a frame for delivery to the connector, preserving order. It is
// the "deliver a frame" half of driving the double.
//
// It returns once the frame is queued, not once it is on the wire. net.Pipe is
// synchronous, so writing inline would mean the relay's read loop blocks
// whenever the connector under test is not currently reading — and a fake that
// deadlocks on the ordering a real relay tolerates tests the wrong thing. A
// write that fails afterwards is recorded in [Link.Errors].
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

// write drains the send queue onto the pipe in order.
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
		// Drain anything queued before the hangup, so a test can still
		// assert on the last frame a connector sent before it died.
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
// restart or a dropped route looks from the connector's side. It is
// idempotent.
func (l *Link) Drop() { l.closeOnce.Do(func() { _ = l.srv.Close() }) }

// Instance returns the instance this link bound during hello, or "" if it has
// not identified itself.
func (l *Link) Instance() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.instance
}

// Errors returns the decode and framing errors the relay saw on this link. A
// connector that emits a malformed frame does not fail a test on its own; this
// is where a test goes looking for why.
func (l *Link) Errors() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// Route delivers f to whichever link bound f.Instance during hello. It is the
// property the envelope exists for: the relay picks a destination from the
// envelope alone and never unmarshals the payload to do it.
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
