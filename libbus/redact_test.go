package libbus

import (
	"log"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_RedactURL_MasksACredentialInEitherUserinfoPosition(t *testing.T) {
	for position, raw := range map[string]string{
		"username": "nats://topsecret@bus:4222",
		"password": "nats://contenox:topsecret@bus:4222",
		"userless": "nats://:topsecret@bus:4222",
	} {
		t.Run(position, func(t *testing.T) {
			out := redactURL(raw)
			require.NotContains(t, out, "topsecret")
			require.Contains(t, out, maskedCredential)
			require.Contains(t, out, "bus:4222", "the server it names stays readable")
		})
	}
}

func TestUnit_RedactURL_MasksEveryEntryOfAServerList(t *testing.T) {
	out := redactURL("nats://topsecret@a:4222,nats://topsecret@b:4222")
	require.NotContains(t, out, "topsecret")
	require.Contains(t, out, "a:4222")
	require.Contains(t, out, "b:4222")
}

// A password containing '/', '?' or '#' must not defeat the userinfo scan:
// those characters are also URL delimiters, so a naive left-to-right scan for
// the end of the authority can land inside the password and hide the real
// '@' from view, leaving the credential unmasked.
func TestUnit_RedactURL_MasksAPasswordContainingURLDelimiters(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		leaked  string
		surface string
	}{
		"slash": {"nats://contenox:top/secret@bus:4222", "top/secret", "bus:4222"},
		"query": {"nats://contenox:top?secret@bus:4222", "top?secret", "bus:4222"},
		"hash":  {"nats://contenox:top#secret@bus:4222", "top#secret", "bus:4222"},
	} {
		t.Run(name, func(t *testing.T) {
			out := redactURL(tc.raw)
			require.NotContains(t, out, tc.leaked)
			require.Contains(t, out, maskedCredential)
			require.Contains(t, out, tc.surface, "the server it names stays readable")
		})
	}
}

func TestUnit_RedactURL_LeavesAURLWithoutUserinfoAlone(t *testing.T) {
	for _, raw := range []string{"nats://bus:4222", "nats://a:4222,nats://b:4222"} {
		require.Equal(t, raw, redactURL(raw))
	}
}

// NewPubSub logs through the standard "log" package (see nats.go), which a
// caller redirecting slog output does not touch, so the connect-failure log
// line must never carry the raw credential itself.
func TestUnit_NewPubSub_ConnectFailureLogDoesNotEchoThePassword(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	_, err = NewPubSub(t.Context(), &Config{NATSURL: "nats://contenox:topsecret@" + addr})
	require.Error(t, err)
	require.NotContains(t, buf.String(), "topsecret")
	require.Contains(t, buf.String(), maskedCredential)
}
