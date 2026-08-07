package relaylink

import (
	"context"
	"crypto/tls"
	"net"
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

// dialTimeout bounds one connection attempt. Unbounded is wrong for the same
// reason the handshake is bounded: a dial that hangs is a connector that has
// stopped retrying without saying so.
const dialTimeout = 15 * time.Second

// DialTLS is the default dialer: TLS over TCP, host verified against the
// endpoint's own name. It is the whole of the transport story, per the decision
// that the relay reads frames and TLS provides confidentiality.
//
// It ignores Credentials. Presenting them — and pinning
// [Credentials.RelayPublicKey] against the peer's long-lived key rather than
// the rotating TLS leaf — belongs with the pairing step that produces them, and
// a dialer that half-checked an identity would be worse than one that visibly
// does not.
func DialTLS(ctx context.Context, endpoint string, _ Credentials) (net.Conn, error) {
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{MinVersion: tls.VersionTLS13},
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return d.DialContext(ctx, "tcp", endpoint)
}
