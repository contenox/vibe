package relaylink_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaylink"
	"github.com/contenox/contenox/internal/relaytest"
	"github.com/contenox/contenox/librelay"
)

// dials counts the connections the connector asked this dialer for. It is how
// a test tells "gave up" from "kept retrying quietly".
func (d *relayDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.links)
}

// helloOf reads the hello a connector sent on l and returns its payload.
func helloOf(t *testing.T, l *relaytest.Link) librelay.Hello {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	f, err := l.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv hello: %v", err)
	}
	if f.Type != librelay.TypeHello {
		t.Fatalf("first frame is %q, want a hello", f.Type)
	}
	var h librelay.Hello
	if err := f.DecodePayload(&h); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	return h
}

// TestUnit_PinnedRelayKeyVerifiesAndConnects is the happy path of relay
// authentication: the relay signs the connector's nonce, the connector checks
// it against the key it pinned at pairing, and the link is held.
func TestUnit_PinnedRelayKeyVerifiesAndConnects(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	cfg.Credentials = relaylink.Credentials{Token: "instance-token", RelayPublicKey: r.PublicKey()}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })

	h := helloOf(t, d.link(0))
	if len(h.Nonce) != librelay.NonceSize {
		t.Fatalf("hello nonce is %d bytes, want %d", len(h.Nonce), librelay.NonceSize)
	}
}

// TestUnit_NonceIsFreshPerConnection proves a signature captured from one
// session cannot be replayed into the next: the challenge changes every dial.
func TestUnit_NonceIsFreshPerConnection(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	cfg.Credentials = relaylink.Credentials{RelayPublicKey: r.PublicKey()}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the first connection", func() bool { return c.Status().Connections == 1 })
	first := helloOf(t, d.link(0))

	d.link(0).Drop()
	waitFor(t, "the second connection", func() bool { return c.Status().Connections == 2 })
	waitFor(t, "the second link", func() bool { return d.link(1) != nil })
	second := helloOf(t, d.link(1))

	if bytes.Equal(first.Nonce, second.Nonce) {
		t.Fatal("the same nonce was offered twice: a welcome signature would replay")
	}
}

// TestUnit_WrongRelayKeyIsFatalAndNotRetried is the failure the pinning exists
// for: something answered the dial and signed the handshake, but not with the
// key this machine paired with. Retrying cannot change that answer, so the
// connector must stop rather than redial forever.
func TestUnit_WrongRelayKeyIsFatalAndNotRetried(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
	// A second relay is a second identity; nothing else about it differs.
	impostorKey := relaytest.New().PublicKey()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	cfg.Credentials = relaylink.Credentials{RelayPublicKey: impostorKey}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "the relay to be refused", func() bool { return c.Status().State == relaylink.StateFatal })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrRelayIdentity) || !errors.Is(err, librelay.ErrBadSignature) {
		t.Fatalf("LastError = %v, want ErrRelayIdentity wrapping ErrBadSignature", err)
	}
	if got := c.Status().Connections; got != 0 {
		t.Fatalf("Connections = %d: an unverified relay is not a connection", got)
	}
	// The whole point of fatal: no redial storm against a peer that cannot
	// prove itself.
	time.Sleep(50 * time.Millisecond)
	if got := d.dials(); got != 1 {
		t.Fatalf("dials = %d after an identity failure, want 1", got)
	}
}

// TestUnit_UnsignedWelcomeIsFatalWhenAKeyIsPinned covers the case a wrong-key
// test does not: a relay that signs nothing at all reaches the verifier by a
// different path, and "no signature" must be as fatal as a bad one — otherwise
// omitting the signature would be a way around the check.
func TestUnit_UnsignedWelcomeIsFatalWhenAKeyIsPinned(t *testing.T) {
	t.Parallel()
	r := relaytest.New(relaytest.NoSignature())
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	cfg.Credentials = relaylink.Credentials{RelayPublicKey: r.PublicKey()}
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "the unsigned welcome to be refused", func() bool { return c.Status().State == relaylink.StateFatal })
	if err := c.Status().LastError; !errors.Is(err, relaylink.ErrRelayIdentity) || !errors.Is(err, librelay.ErrNoSignature) {
		t.Fatalf("LastError = %v, want ErrRelayIdentity wrapping ErrNoSignature", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := d.dials(); got != 1 {
		t.Fatalf("dials = %d after an identity failure, want 1", got)
	}
}

// TestUnit_NoCredentialsBehavesExactlyAsBefore is the regression guard for
// every runtime that has never run `contenox login`: with nothing pinned there
// is nothing to verify, an unsigned welcome is accepted, and the connector
// behaves as it did before relay authentication existed.
func TestUnit_NoCredentialsBehavesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	r := relaytest.New(relaytest.NoSignature())
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
	// The zero Credentials: no token, no pinned key.
	c, err := relaylink.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connected", func() bool { return c.Status().State == relaylink.StateConnected })
	if err := c.Status().LastError; err != nil {
		t.Fatalf("LastError = %v on an unauthenticated connection, want none", err)
	}
}

// TestUnit_NewRejectsAnUnusableRelayKey keeps a key that cannot be parsed a
// configuration error the caller sees at construction, rather than a fatal
// state the retry loop discovers later and attributes to the relay.
func TestUnit_NewRejectsAnUnusableRelayKey(t *testing.T) {
	t.Parallel()
	cfg := baseConfig()
	cfg.Dial = func(context.Context, string, relaylink.Credentials) (net.Conn, error) {
		t.Error("New must not dial")
		return nil, errors.New("unreachable")
	}
	cfg.Credentials = relaylink.Credentials{RelayPublicKey: "not-a-key"}
	if _, err := relaylink.New(cfg); !errors.Is(err, librelay.ErrBadPublicKey) {
		t.Fatalf("New = %v, want ErrBadPublicKey", err)
	}
}
