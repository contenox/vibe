package relaylink

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DialFunc opens one connection to a relay. It is the connector's only contact
// with the network, and it dials — there is no accept anywhere in this package
// and no way to add one through this interface.
//
// The endpoint is passed through uninterpreted, so a deployment may address a
// relay however it likes. Credentials are passed so a dialer can present them
// at the transport layer; the connector never puts them in a frame.
type DialFunc func(ctx context.Context, endpoint string, creds Credentials) (net.Conn, error)

// dialTimeout bounds one connection attempt, from TCP through the upgrade.
// Unbounded is wrong for the same reason the handshake is bounded: a dial that
// hangs is a connector that has stopped retrying without saying so.
const dialTimeout = 15 * time.Second

// UpgradeProtocol is the token this build names in its Upgrade header, and the
// only token it will accept back. The suffix versions the framing, not the
// envelope — librelay negotiates its own version in the handshake — so a future
// framing change is refused at the upgrade rather than discovered as garbage on
// the stream.
const UpgradeProtocol = "contenox-relay/1"

// defaultConnectPath is where a relay serves the connection endpoint when the
// configured endpoint names only a host. It is a default and not a constant of
// the protocol: an endpoint may carry any path.
const defaultConnectPath = "/v1/connect"

// DialTLS is the default dialer: an HTTP/1.1 upgrade over TLS.
//
// An upgrade rather than a bare TCP port because a relay is reached through one
// HTTPS endpoint — one hostname, one certificate, and the only port that
// survives a hotel network — and because it is what carries the credential.
// [Credentials.Token] is presented as `Authorization: Bearer`, on the upgrade
// request and nowhere else, so it never enters a frame and never reaches a
// component that routes one.
//
// It does not check [Credentials.RelayPublicKey]: the relay proves itself
// inside the librelay handshake, against a long-lived key, precisely so that
// nothing here depends on the TLS leaf — that certificate rotates roughly every
// 90 days and a pin on it would expire itself in the field.
func DialTLS(ctx context.Context, endpoint string, creds Credentials) (net.Conn, error) {
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{MinVersion: tls.VersionTLS13},
	}
	conn, err := d.DialContext(ctx, "tcp", addrOf(u))
	if err != nil {
		return nil, err
	}
	up, err := upgrade(ctx, conn, u, creds)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return up, nil
}

// parseEndpoint reads the configured endpoint. A bare host or host:port is
// read as https, because there is no other transport: a relay endpoint that
// silently fell back to cleartext would put the bearer credential on the wire
// in plain text, and that is not a mistake configuration should be able to
// make.
func parseEndpoint(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("relaylink: endpoint is empty")
	}
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("relaylink: endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("relaylink: endpoint %q: scheme must be https", endpoint)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("relaylink: endpoint %q: no host", endpoint)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = defaultConnectPath
	}
	return u, nil
}

// addrOf is the dial address, defaulting to the https port.
func addrOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// upgrade performs the HTTP/1.1 upgrade on an established connection and
// returns the connection to speak librelay over.
//
// It is separate from DialTLS so it can be exercised over an in-memory pipe:
// this package must never open a listening socket, not even in a test, so the
// only way to test the exchange is to be able to drive both ends of a pipe.
func upgrade(ctx context.Context, conn net.Conn, u *url.URL, creds Credentials) (net.Conn, error) {
	// The upgrade gets its own deadline, cleared on success. A live
	// connection must not inherit it, for the same reason the librelay
	// handshake clears its own: a connection that dies on a schedule
	// unrelated to liveness looks like a flapping relay.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("relaylink: set upgrade deadline: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("relaylink: build upgrade request: %w", err)
	}
	req.Header.Set("Upgrade", UpgradeProtocol)
	req.Header.Set("Connection", "Upgrade")
	if creds.Token != "" {
		// Empty is legal and deliberately not an error here: whether an
		// unauthenticated connector is admitted is the relay's decision,
		// and a dialer that refused to try would take it away.
		req.Header.Set("Authorization", "Bearer "+creds.Token)
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("relaylink: send upgrade request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("relaylink: read upgrade response: %w", err)
	}
	// A 1xx carries no body; closing is form, not a read.
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
	case http.StatusUnauthorized, http.StatusForbidden:
		// Fatal, via the connector's retry policy: a refused credential
		// does not repair itself, and a revoked instance that kept
		// dialing would be a reconnect storm from a machine that has
		// been taken away.
		return nil, fmt.Errorf("%w: relay answered %s", ErrUnauthorized, resp.Status)
	default:
		return nil, fmt.Errorf("relaylink: relay refused the upgrade: %s", resp.Status)
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("Upgrade")), UpgradeProtocol) {
		// Refusing, not permitting, so an exact match is the safe
		// direction to err in.
		return nil, fmt.Errorf("relaylink: relay upgraded to %q, this build speaks %q",
			resp.Header.Get("Upgrade"), UpgradeProtocol)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("relaylink: clear upgrade deadline: %w", err)
	}
	// Always over the buffered reader: the response parse may have pulled
	// in bytes of whatever the relay sent immediately after the 101, and
	// reading the socket directly from here would drop them.
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn is a connection whose reads are served from a bufio.Reader that
// already holds bytes taken off the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

// Read serves buffered bytes before the socket's.
func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
