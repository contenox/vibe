package relaylink

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func upgradeOver(t *testing.T, creds Credentials, answer func(*http.Request) string) (net.Conn, error) {
	t.Helper()
	peer, mine := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	got := make(chan struct{})
	go func() {
		defer close(got)
		br := bufio.NewReader(peer)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.WriteString(peer, answer(req))
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	u := &url.URL{Scheme: "https", Host: "relay.invalid", Path: defaultConnectPath}
	conn, err := upgrade(ctx, mine, u, creds)
	<-got
	return conn, err
}

const okUpgrade = "HTTP/1.1 101 Switching Protocols\r\n" +
	"Upgrade: " + UpgradeProtocol + "\r\n" +
	"Connection: Upgrade\r\n\r\n"

// TestUnit_UpgradePresentsTheInstanceTokenAsBearer checks the credential
// rides on the upgrade request only, never in a frame.
func TestUnit_UpgradePresentsTheInstanceTokenAsBearer(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	conn, err := upgradeOver(t, Credentials{Token: "sekrit"}, func(r *http.Request) string {
		seen = r
		return okUpgrade
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := seen.Header.Get("Authorization"); got != "Bearer sekrit" {
		t.Fatalf("Authorization = %q, want a bearer credential", got)
	}
	if got := seen.Header.Get("Upgrade"); got != UpgradeProtocol {
		t.Fatalf("Upgrade = %q, want %q", got, UpgradeProtocol)
	}
	if got := seen.URL.Path; got != defaultConnectPath {
		t.Fatalf("path = %q, want %q", got, defaultConnectPath)
	}
}

// TestUnit_UpgradeWithoutATokenStillTries checks an empty token still
// attempts the upgrade, leaving admission to the relay.
func TestUnit_UpgradeWithoutATokenStillTries(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	conn, err := upgradeOver(t, Credentials{}, func(r *http.Request) string {
		seen = r
		return okUpgrade
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := seen.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q on an unauthenticated dial, want none", got)
	}
}

// TestUnit_UpgradeRefusalIsUnauthorized checks a refused credential maps to
// ErrUnauthorized and the connector's fatal set.
func TestUnit_UpgradeRefusalIsUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := upgradeOver(t, Credentials{Token: "revoked"}, func(*http.Request) string {
		return "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("upgrade = %v, want ErrUnauthorized", err)
	}
	if !isFatal(err) {
		t.Fatal("a refused credential must be fatal")
	}
}

// TestUnit_UpgradeToAnotherProtocolIsRefused checks an Upgrade response
// naming a different protocol is refused.
func TestUnit_UpgradeToAnotherProtocolIsRefused(t *testing.T) {
	t.Parallel()
	_, err := upgradeOver(t, Credentials{}, func(*http.Request) string {
		return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	})
	if err == nil {
		t.Fatal("upgrade accepted a protocol this build does not speak")
	}
}

// TestUnit_UpgradeKeepsBytesPipelinedBehindTheResponse checks bytes flushed
// right after the 101 are not lost to the response parser's buffer.
func TestUnit_UpgradeKeepsBytesPipelinedBehindTheResponse(t *testing.T) {
	t.Parallel()
	const trailing = "{\"t\":\"relay.welcome\"}\n"
	conn, err := upgradeOver(t, Credentials{}, func(*http.Request) string {
		return okUpgrade + trailing
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, len(trailing))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read pipelined bytes: %v", err)
	}
	if string(buf) != trailing {
		t.Fatalf("read %q, want %q", buf, trailing)
	}
}

// TestUnit_ParseEndpointRefusesCleartext checks a bare endpoint defaults to
// https and rejects cleartext schemes.
func TestUnit_ParseEndpointRefusesCleartext(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "http://relay.invalid", "ws://relay.invalid", "https:///nohost"} {
		if _, err := parseEndpoint(bad); err == nil {
			t.Errorf("parseEndpoint(%q) was accepted", bad)
		}
	}
	for _, tc := range []struct{ in, host, path string }{
		{"relay.invalid", "relay.invalid:443", defaultConnectPath},
		{"relay.invalid:8443", "relay.invalid:8443", defaultConnectPath},
		{"https://relay.invalid/", "relay.invalid:443", defaultConnectPath},
		{"https://relay.invalid/other", "relay.invalid:443", "/other"},
	} {
		u, err := parseEndpoint(tc.in)
		if err != nil {
			t.Fatalf("parseEndpoint(%q): %v", tc.in, err)
		}
		if got := addrOf(u); got != tc.host {
			t.Errorf("parseEndpoint(%q) address = %q, want %q", tc.in, got, tc.host)
		}
		if u.Path != tc.path {
			t.Errorf("parseEndpoint(%q) path = %q, want %q", tc.in, u.Path, tc.path)
		}
	}
}
