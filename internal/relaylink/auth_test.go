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

func (d *relayDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.links)
}

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

// TestUnit_PinnedRelayKeyVerifiesAndConnects checks the connector verifies a
// welcome signed by the pinned key and holds the link.
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

// TestUnit_NonceIsFreshPerConnection checks the challenge changes every dial,
// so a captured signature cannot be replayed into the next session.
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

// TestUnit_WrongRelayKeyIsFatalAndNotRetried checks a welcome signed by the
// wrong key is refused fatally, without a redial storm.
func TestUnit_WrongRelayKeyIsFatalAndNotRetried(t *testing.T) {
	t.Parallel()
	r := relaytest.New()
	defer r.Close()
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
	time.Sleep(50 * time.Millisecond)
	if got := d.dials(); got != 1 {
		t.Fatalf("dials = %d after an identity failure, want 1", got)
	}
}

// TestUnit_UnsignedWelcomeIsFatalWhenAKeyIsPinned checks an unsigned welcome
// is refused as fatally as a bad signature.
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

// TestUnit_NoCredentialsBehavesExactlyAsBefore checks that with nothing
// pinned, an unsigned welcome is accepted.
func TestUnit_NoCredentialsBehavesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	r := relaytest.New(relaytest.NoSignature())
	defer r.Close()
	d := &relayDialer{relay: r}

	cfg := baseConfig()
	cfg.Dial = d.dial
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

// TestUnit_NewRejectsAnUnusableRelayKey checks an unparsable key fails at
// construction rather than as a retry-loop fatal.
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
