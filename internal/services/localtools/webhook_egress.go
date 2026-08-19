package localtools

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errRedirectBlocked marks a refusal raised from CheckRedirect so doRequest
// surfaces it verbatim instead of burning retries on a hop the policy already
// refused. A blocked hop is never dialed: the refusal fires before the client
// follows the redirect.
var errRedirectBlocked = errors.New("native-web: redirect refused by policy")

// errEgressBlocked marks the transport-level link-local refusal so a name that
// resolves to a metadata address fails once rather than through the retry loop.
var errEgressBlocked = errors.New("native-web: link-local / cloud-metadata address refused")

// isBlockedEgressIP reports whether ip is a link-local / cloud-metadata target
// the toolset must never reach: 169.254.169.254 and the rest of 169.254.0.0/16,
// and IPv6 link-local. Mirrors the SSRF classes libsandbox's egress guard rejects,
// scoped to link-local so loopback and private ranges (on-host services) still resolve.
func isBlockedEgressIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// denyLinkLocalDial is the transport-level net: a hostname that resolves to a
// link-local / metadata address (metadata.google.internal, an attacker A record)
// is refused at connect time, after resolution, where a URL-string host check
// cannot see the address. address is the resolved ip:port about to be dialed.
func denyLinkLocalDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedEgressIP(ip) {
		return fmt.Errorf("%w: %s", errEgressBlocked, host)
	}
	return nil
}

// newContainedWebClient builds the default client with the link-local dial guard
// installed, so every request the toolset makes — first hop and every redirect —
// is checked against the resolved address, not just the URL string.
func newContainedWebClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   denyLinkLocalDial,
	}).DialContext
	return &http.Client{Timeout: timeout, Transport: tr}
}
