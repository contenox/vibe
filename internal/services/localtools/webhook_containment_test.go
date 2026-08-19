package localtools_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/stretchr/testify/require"
)

// TestUnit_WebTools_RedirectHopReValidatesHostPolicy is the containment test for
// the toolset's own reach: the host allow/deny policy that guards the first URL
// is re-applied on every redirect hop. An allowed origin that 302s to a denied
// or link-local host is refused before the hop is dialed — the denied host is
// never contacted, and a same-host redirect still follows normally.
func TestUnit_WebTools_RedirectHopReValidatesHostPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-denied":
			// denied.invalid must be refused ON the hop, before any DNS/dial.
			http.Redirect(w, r, "http://denied.invalid/secret", http.StatusFound)
		case "/redirect-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/redirect-ok":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			_, _ = w.Write([]byte(`{"ok":true,"final":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	tools := newWebTools(t, &recTracker{})

	// Escape refused: a redirect to a denied host is blocked on the hop. The error
	// is the policy message, not a DNS failure — proof the hop was refused pre-dial.
	deniedCtx := ctxWithPolicy(map[string]string{"_denied_hosts": "denied.invalid"})
	_, _, err := execWeb(t, deniedCtx, tools, "web_get", map[string]any{"url": srv.URL + "/redirect-denied"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "_denied_hosts",
		"the denied host must be refused on the redirect hop, before it is dialed")
	require.Contains(t, err.Error(), "redirect refused by policy")

	// Control-plane analog refused: a redirect to the cloud-metadata address is
	// blocked on the hop by the hard link-local rule, regardless of host knobs.
	_, _, err = execWeb(t, deniedCtx, tools, "web_get", map[string]any{"url": srv.URL + "/redirect-metadata"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "link-local / cloud-metadata",
		"a redirect to 169.254.169.254 must be refused on the hop")

	// A same-host redirect the policy allows is still followed to completion.
	res, dt, err := execWeb(t, ctxWithPolicy(map[string]string{"_denied_hosts": ""}),
		tools, "web_get", map[string]any{"url": srv.URL + "/redirect-ok"})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	m, ok := res.(map[string]any)
	require.True(t, ok, "expected JSON map, got %T", res)
	require.Equal(t, true, m["final"], "an allowed redirect must still be followed")
}

// TestUnit_WebTools_LinkLocalTargetRefused pins the cloud-metadata block on the
// first URL: a link-local literal is a soft denial (a result, like a denied host),
// and no request is ever built — there is no server to contact.
func TestUnit_WebTools_LinkLocalTargetRefused(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	ctx := ctxWithPolicy(map[string]string{"_denied_hosts": ""})

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/", // AWS/GCP/Azure cloud metadata
		"http://[fe80::1]/",                        // IPv6 link-local
	} {
		res, dt, err := execWeb(t, ctx, tools, "web_get", map[string]any{"url": target})
		require.NoError(t, err, "a link-local block is a soft denial, not an error: %s", target)
		require.Equal(t, taskengine.DataTypeString, dt, target)
		msg, _ := res.(string)
		require.Contains(t, msg, "link-local / cloud-metadata", target)
	}
}

// TestUnit_WebTools_BlockedEgressPredicate is the predicate itself, mirroring
// libsandbox's SSRF classification but scoped to link-local so loopback and
// private ranges (on-host services, httptest) still resolve.
func TestUnit_WebTools_BlockedEgressPredicate(t *testing.T) {
	blocked := []string{"169.254.169.254", "169.254.0.1", "fe80::1"}
	for _, s := range blocked {
		require.Truef(t, localtools.ExportedIsBlockedEgressIP(net.ParseIP(s)),
			"%s is link-local / cloud-metadata and must be blocked", s)
	}
	allowed := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "8.8.8.8", "::1"}
	for _, s := range allowed {
		require.Falsef(t, localtools.ExportedIsBlockedEgressIP(net.ParseIP(s)),
			"%s is not link-local; on-host and public services must still resolve", s)
	}
	require.False(t, localtools.ExportedIsBlockedEgressIP(nil))
}

// TestUnit_WebTools_SupportsAdvertisesOnlyTheScopedName pins the set contract at
// the Supports() boundary: only the scoped bundle name is advertised, never a
// bare verb, so one allowlist entry addresses all six and "!native-web" removes
// them together.
func TestUnit_WebTools_SupportsAdvertisesOnlyTheScopedName(t *testing.T) {
	tools := newWebTools(t, &recTracker{})
	supported, err := tools.Supports(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{localtools.WebToolsName}, supported)
	require.NotContains(t, supported, "web_get",
		"a bare verb name would be its own allowlist entry and survive \"!native-web\"")
}

// Note: the env-scrub half of the reconnect (libsandbox EnvScrub / resolvedSandboxEnv)
// has no surface here — the web toolset spawns no child process, so there is no
// os.Environ() to scrub. That reconnect belongs to the process-launching tools.
