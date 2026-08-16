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

// DialFunc opens one connection to a relay. It never accepts.
type DialFunc func(ctx context.Context, endpoint string, creds Credentials) (net.Conn, error)

// dialTimeout bounds one connection attempt, from TCP through the upgrade.
const dialTimeout = 15 * time.Second

// UpgradeProtocol is the token this build names in its Upgrade header, and the
// only one it accepts back.
const UpgradeProtocol = "contenox-relay/1"

// defaultConnectPath is where a relay serves the connection endpoint when the
// configured endpoint names only a host.
const defaultConnectPath = "/v1/connect"

// DialTLS is the default dialer: an HTTP/1.1 upgrade over TLS, presenting
// [Credentials.Token] as `Authorization: Bearer` on the upgrade request only.
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

func parseEndpoint(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("relaylink: endpoint is empty")
	}
	raw := endpoint
	// A bare host or host:port is read as https; a cleartext fallback would put
	// the bearer credential on the wire in plain text.
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

func addrOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func upgrade(ctx context.Context, conn net.Conn, u *url.URL, creds Credentials) (net.Conn, error) {
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
	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusSwitchingProtocols:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: relay answered %s", ErrUnauthorized, resp.Status)
	default:
		return nil, fmt.Errorf("relaylink: relay refused the upgrade: %s", resp.Status)
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Header.Get("Upgrade")), UpgradeProtocol) {
		return nil, fmt.Errorf("relaylink: relay upgraded to %q, this build speaks %q",
			resp.Header.Get("Upgrade"), UpgradeProtocol)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("relaylink: clear upgrade deadline: %w", err)
	}
	// Always over the buffered reader: the response parse may have pulled in
	// bytes the relay sent right after the 101.
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
