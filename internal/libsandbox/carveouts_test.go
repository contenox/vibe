package libsandbox_test

import (
	"strings"
	"testing"

	"github.com/contenox/beam/internal/libsandbox"
	"github.com/stretchr/testify/require"
)

// Deny-default: an empty reader allows nothing, and that is not an error.
func TestUnit_LoadCarveouts_DenyDefaultOnEmptyReader(t *testing.T) {
	fs, net, err := libsandbox.LoadCarveouts(strings.NewReader(""))

	require.NoError(t, err)
	require.Empty(t, fs)
	require.Empty(t, net)
}

// An empty object or "null" document likewise allows nothing.
func TestUnit_LoadCarveouts_DenyDefaultOnEmptyObject(t *testing.T) {
	for _, doc := range []string{`{}`, `null`} {
		fs, net, err := libsandbox.LoadCarveouts(strings.NewReader(doc))
		require.NoErrorf(t, err, "doc %q", doc)
		require.Emptyf(t, fs, "doc %q", doc)
		require.Emptyf(t, net, "doc %q", doc)
	}
}

// A well-formed necessity list decodes into the exact carve-outs, mode and
// justification preserved.
func TestUnit_LoadCarveouts_AcceptsValidDoc(t *testing.T) {
	doc := `{
	  "filesystem": [ {"path":"~/.claude","mode":"ro","needs":"agent auth/config to start"} ],
	  "network":    [ {"host":"registry.npmjs.org","needs":"npm install fetch"} ]
	}`

	fs, net, err := libsandbox.LoadCarveouts(strings.NewReader(doc))

	require.NoError(t, err)
	require.Equal(t, []libsandbox.FSCarveout{
		{Path: "~/.claude", Mode: libsandbox.ModeRO, Needs: "agent auth/config to start"},
	}, fs)
	require.Equal(t, []libsandbox.NetCarveout{
		{Host: "registry.npmjs.org", Needs: "npm install fetch"},
	}, net)
}

// An unknown field (a typo, a smuggled key) fails loudly rather than silently
// changing a hole.
func TestUnit_LoadCarveouts_RejectsUnknownField(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"filesystem":[{"path":"~/.claude","mode":"ro","needs":"x","extra":"nope"}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

func TestUnit_LoadCarveouts_RejectsBadMode(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"filesystem":[{"path":"/x","mode":"rwx","needs":"x"}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

// A hole with no justification is not admitted.
func TestUnit_LoadCarveouts_RejectsEmptyNeeds(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"filesystem":[{"path":"/x","mode":"ro","needs":""}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

func TestUnit_LoadCarveouts_RejectsEmptyPath(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"filesystem":[{"path":"","mode":"ro","needs":"x"}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

func TestUnit_LoadCarveouts_RejectsTraversalPath(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"filesystem":[{"path":"~/../etc","mode":"ro","needs":"x"}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

func TestUnit_LoadCarveouts_RejectsEmptyHost(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(
		`{"network":[{"host":"","needs":"x"}]}`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}

// Malformed JSON is reported as an invalid carve-out, not a bare decode error.
func TestUnit_LoadCarveouts_RejectsMalformedJSON(t *testing.T) {
	_, _, err := libsandbox.LoadCarveouts(strings.NewReader(`{"filesystem": [`))

	require.ErrorIs(t, err, libsandbox.ErrInvalidCarveout)
}
