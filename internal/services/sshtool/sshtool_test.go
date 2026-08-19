package sshtool_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/sshtool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnit_SSHTools_RejectsUnknownArgsBeforeDial is the archived guard: an
// argument the tool does not accept is refused rather than quietly dropped, and
// it is refused before anything is dialled.
func TestUnit_SSHTools_RejectsUnknownArgsBeforeDial(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	before := len(srv.commands())

	args := srv.callArgs("greet")
	args["unexpected"] = true

	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, srv.knownHosts(t)), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown argument")
	assert.Contains(t, err.Error(), "unexpected")
	assert.Len(t, srv.commands(), before, "an unknown argument still reached the remote host")
}

// strict_host_key was accepted and then ignored by the archived tool, which told
// the model verification was negotiable when it never was. It is refused now, so
// the descriptor and the behaviour agree.
func TestUnit_SSHTools_HostKeyVerificationIsNotAnArgument(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	args := srv.callArgs("greet")
	args["strict_host_key"] = false

	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, srv.knownHosts(t)), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strict_host_key")
	assert.Empty(t, srv.commands(), "a call that tried to turn off verification still ran")
}

func TestUnit_SSHTools_RequiredArguments(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")
	ctx := withPolicy(map[string]string{"_allowed_hosts": "build.example.com"})

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"host", map[string]any{"user": "deploy", "command": "uptime"}, "`host` is required"},
		{"user", map[string]any{"host": "build.example.com", "command": "uptime"}, "`user` is required"},
		{"command", map[string]any{"host": "build.example.com", "user": "deploy"}, "`command` is required"},
		{"port range", map[string]any{"host": "build.example.com", "user": "deploy", "command": "uptime", "port": 0}, "not between 1 and 65535"},
		{"fingerprint shape", map[string]any{"host": "build.example.com", "user": "deploy", "command": "uptime", "host_key": "abc123"}, "not a SHA256 fingerprint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := execOn(ctx, repo, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestUnit_SSHTools_ExecutesRemoteCommand(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	repo := newTools(t, srv.knownHosts(t))

	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_hosts": srv.host}), repo, srv.callArgs("greet"))
	assert.True(t, res.Success)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello from the remote", res.Stdout)
	assert.Empty(t, res.Stderr)
	assert.Equal(t, "greet", res.Command)
	assert.Equal(t, srv.host, res.Host)
	assert.Equal(t, testUser, res.User)
	assert.False(t, res.Truncated)
	// The archived tool declared host_key in its result and never populated it;
	// a fingerprint nothing reports cannot be pinned on the next call.
	assert.Equal(t, srv.fingerprint(), res.HostKey, "the result does not report the key verification accepted")
	assert.Equal(t, []string{"greet"}, srv.commands())
}

// A non-zero exit is an ANSWER. The archived tool threw the populated result
// away and returned a bare Go error, so the model never saw stderr.
func TestUnit_SSHTools_NonZeroExitIsAResultNotAnError(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, srv.knownHosts(t)), srv.callArgs("fail"))
	assert.False(t, res.Success)
	assert.Equal(t, 3, res.ExitCode)
	assert.Equal(t, "boom", res.Stderr)
	assert.Contains(t, res.Error, "status 3")
}

func TestUnit_SSHTools_UnknownHostKeyIsRefused(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	// An empty known_hosts: the server is real and reachable, and still refused.
	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, ""), srv.callArgs("greet"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "known_hosts")
	assert.Empty(t, srv.commands(), "an unverified host key still ran a command")
}

// A `host_key` pin can only narrow what known_hosts already accepts. The
// archived tool parsed the argument and never used it, so a pinned call was
// silently unpinned.
func TestUnit_SSHTools_HostKeyPinNarrowsButNeverWidens(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	known := srv.knownHosts(t)
	ctx := withPolicy(map[string]string{"_allowed_hosts": srv.host})

	t.Run("a matching pin passes", func(t *testing.T) {
		t.Parallel()
		args := srv.callArgs("greet")
		args["host_key"] = srv.fingerprint()
		res := mustExecOn(t, ctx, newTools(t, known), args)
		assert.True(t, res.Success)
	})

	t.Run("a mismatched pin is refused even though known_hosts accepts the key", func(t *testing.T) {
		t.Parallel()
		args := srv.callArgs("greet")
		args["host_key"] = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		_, _, err := execOn(ctx, newTools(t, known), args)
		require.Error(t, err)
		assert.ErrorIs(t, err, sshtool.ErrHostKeyMismatch)
	})

	t.Run("a pin does not substitute for known_hosts", func(t *testing.T) {
		t.Parallel()
		args := srv.callArgs("greet")
		args["host_key"] = srv.fingerprint()
		_, _, err := execOn(ctx, newTools(t, ""), args)
		require.Error(t, err, "a pinned fingerprint stood in for an empty known_hosts")
		assert.Contains(t, err.Error(), "known_hosts")
	})
}

// A KNOWN host presenting a DIFFERENT key is the case host key checking exists
// for, so it is refused as fatal and named as what it is, not reported as a
// missing entry the model might try to add.
func TestUnit_SSHTools_ChangedHostKeyIsFatal(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	impostor := newRemote(t)

	// known_hosts records the impostor's key for the address we dial.
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhostsLine(t, srv.addr(), impostor)
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))

	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, path), srv.callArgs("greet"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DIFFERENT key")
	assert.Contains(t, err.Error(), "fatal:")
	assert.Empty(t, srv.commands(), "a host whose key changed was connected to anyway")
}

// A hashed known_hosts file is what OpenSSH writes by default; the archived
// parser skipped those lines, which refused every host on an ordinary machine.
func TestUnit_SSHTools_HashedKnownHostsEntriesAreHonoured(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	path := filepath.Join(t.TempDir(), "known_hosts")
	plain := srv.knownHosts(t)
	data, err := os.ReadFile(plain)
	require.NoError(t, err)
	fields := strings.Fields(string(data))
	require.Len(t, fields, 3)
	hashed := strings.Join([]string{hashHost(t, fields[0]), fields[1], fields[2]}, " ")
	require.NoError(t, os.WriteFile(path, []byte(hashed+"\n"), 0o600))

	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, path), srv.callArgs("greet"))
	assert.True(t, res.Success)
}

func TestUnit_SSHTools_OutputIsCappedAtTheContextBudget(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	ctx := context.WithValue(withPolicy(map[string]string{"_allowed_hosts": srv.host}),
		taskengine.ContextKeyOutputByteLimit, int64(1024))
	res := mustExecOn(t, ctx, newTools(t, srv.knownHosts(t)), srv.callArgs("flood"))

	assert.True(t, res.Truncated)
	assert.False(t, res.Success)
	assert.Contains(t, res.Error, "truncated")
	// Never silent, and never empty: the kept bytes are a clean head of the stream.
	assert.NotEmpty(t, res.Stdout)
	assert.LessOrEqual(t, len(res.Stdout), 1024)
}

func TestUnit_SSHTools_TimeoutIsAResultNotAHang(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	args := srv.callArgs("hang")
	args["timeout"] = "300ms"

	start := time.Now()
	res := mustExecOn(t, withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, srv.knownHosts(t)), args)
	assert.Less(t, time.Since(start), 10*time.Second, "the timeout did not cut the command off")
	assert.False(t, res.Success)
	assert.Equal(t, -1, res.ExitCode)
	assert.Contains(t, res.Error, "did not finish within")
}

func TestUnit_SSHTools_AuthenticationIsRequired(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	ctx := withPolicy(map[string]string{"_allowed_hosts": srv.host})

	args := srv.callArgs("greet")
	delete(args, "password")
	_, _, err := execOn(ctx, newTools(t, srv.knownHosts(t)), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no authentication method")

	args = srv.callArgs("greet")
	args["password"] = "wrong"
	_, _, err = execOn(ctx, newTools(t, srv.knownHosts(t)), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot connect")
}

// The pool must never hand a session to a caller whose credentials differ from
// the ones that opened the connection.
func TestUnit_SSHTools_ClientCacheReusesOneConnectionPerIdentity(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)
	repo := newTools(t, srv.knownHosts(t), sshtool.WithClientCache())
	ctx := withPolicy(map[string]string{"_allowed_hosts": srv.host})

	first := mustExecOn(t, ctx, repo, srv.callArgs("greet"))
	second := mustExecOn(t, ctx, repo, srv.callArgs("greet"))
	assert.True(t, first.Success)
	assert.True(t, second.Success)
	// A pooled connection never re-runs the host key callback, so the result
	// still has to report the fingerprint the handshake accepted.
	assert.Equal(t, srv.fingerprint(), second.HostKey)

	bad := srv.callArgs("greet")
	bad["password"] = "wrong"
	_, _, err := execOn(ctx, repo, bad)
	require.Error(t, err, "the pool handed a session to the wrong credentials")

	closer, ok := repo.(interface{ Close() error })
	require.True(t, ok)
	require.NoError(t, closer.Close())
}

func TestUnit_SSHTools_ConstructorRefusesWhatItCannotVerify(t *testing.T) {
	t.Parallel()

	_, err := sshtool.NewSSHTools(sshtool.WithKnownHostsFile(filepath.Join(t.TempDir(), "absent")))
	require.Error(t, err, "a known_hosts file that does not exist was accepted")

	_, err = sshtool.NewSSHTools(sshtool.WithAllowedHosts("*"))
	require.Error(t, err, "the operator ceiling accepted a wildcard")

	_, err = sshtool.NewSSHTools(sshtool.WithDefaultPort(0))
	require.Error(t, err)

	_, err = sshtool.NewSSHTools(sshtool.WithCustomHostKeyCallback(nil))
	require.Error(t, err)

	// WithStrictHostKey no longer depends on being ordered after
	// WithKnownHostsFile: the verifier is built once, after every option ran.
	known := filepath.Join(t.TempDir(), "known_hosts")
	require.NoError(t, os.WriteFile(known, nil, 0o600))
	_, err = sshtool.NewSSHTools(sshtool.WithStrictHostKey(), sshtool.WithKnownHostsFile(known))
	require.NoError(t, err)
}

// A key file readable beyond its owner is refused rather than used, and the
// refusal is fatal: no retry fixes the mode.
func TestUnit_SSHTools_PermissivePrivateKeyFileIsRefused(t *testing.T) {
	t.Parallel()
	srv := newRemote(t)

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("not a key"), 0o644))

	args := srv.callArgs("greet")
	delete(args, "password")
	args["private_key_file"] = keyPath

	_, _, err := execOn(withPolicy(map[string]string{"_allowed_hosts": srv.host}), newTools(t, srv.knownHosts(t)), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "readable beyond its owner")
	assert.Contains(t, err.Error(), "fatal:")
	assert.Empty(t, srv.commands())
}

func TestUnit_SSHTools_UnknownToolIsRefused(t *testing.T) {
	t.Parallel()
	repo := newTools(t, "")

	_, _, err := repo.Exec(context.Background(), time.Now(), map[string]any{"host": "a", "user": "b", "command": "c"}, false,
		&taskengine.ToolsCall{Name: sshtool.ToolsProviderName, ToolName: "delete_everything"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")

	_, _, err = repo.Exec(context.Background(), time.Now(), nil, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tools required")
}
