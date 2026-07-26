package acpexec_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/contenox/beam/libacp/acpexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise acpexec.Spawn/Process against trivial, always-present
// subprocesses (cat, echo, sleep) rather than the ACP reference binaries —
// they validate the transport in isolation, independent of anything ACP- or
// testy-specific (see e2e_testy_test.go for that).

func TestSpawn_EchoesStdinToStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := acpexec.Spawn(ctx, exec.Command("cat"))
	require.NoError(t, err)

	_, err = proc.Write([]byte("hello\n"))
	require.NoError(t, err)

	r := bufio.NewReader(proc)
	line, err := r.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "hello\n", line)

	require.NoError(t, proc.Close())
}

func TestSpawn_ProcessExitingOnItsOwnYieldsEOFAndCloseDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := acpexec.Spawn(ctx, exec.Command("sh", "-c", "echo from-child"))
	require.NoError(t, err)

	out, err := io.ReadAll(proc)
	require.NoError(t, err)
	assert.Equal(t, "from-child\n", string(out))

	closeDone := make(chan error, 1)
	go func() { closeDone <- proc.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung after the subprocess had already exited on its own")
	}
}

func TestSpawn_DoubleCloseIsSafeAndReturnsSameResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := acpexec.Spawn(ctx, exec.Command("cat"))
	require.NoError(t, err)

	err1 := proc.Close()
	err2 := proc.Close()
	assert.NoError(t, err1)
	assert.Equal(t, err1, err2)
}

func TestSpawn_CloseKillsAProcessThatIgnoresStdinClosing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// sleep never reads stdin, so closing it (Close's first step) cannot make
	// sleep exit on its own; Close must fall through to Process.Kill once the
	// (short, test-only) grace period elapses.
	proc, err := acpexec.Spawn(ctx, exec.Command("sleep", "30"), acpexec.WithKillGrace(200*time.Millisecond))
	require.NoError(t, err)

	closeDone := make(chan error, 1)
	start := time.Now()
	go func() { closeDone <- proc.Close() }()
	select {
	case err := <-closeDone:
		assert.Less(t, time.Since(start), 10*time.Second, "Close should have killed the process well within its 30s sleep")
		// The kill is the escalation tail of Close's own documented shutdown
		// sequence, so the kill-induced exit status must not surface as a
		// Close error (persistent agents like testy always take this path).
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not kill a subprocess ignoring stdin closing")
	}
}

// TestSpawn_CloseKillsTheWholeProcessTree reproduces the npx-wrapper shape
// that real registry agents spawn as (`npx -y <package>` forks the actual
// agent a level down): a shell whose backgrounded child inherits our
// stdout/stderr pipes. Killing only the direct child would leave that
// grandchild alive holding the pipes, blocking the Wait reaper — and Close —
// forever. With the process group in place, Close must take the whole tree
// down and return promptly and cleanly.
func TestSpawn_CloseKillsTheWholeProcessTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	proc, err := acpexec.Spawn(ctx,
		exec.Command("sh", "-c", "sleep 300 & exec sleep 300"),
		acpexec.WithStderr(&stderr),
		acpexec.WithKillGrace(200*time.Millisecond))
	require.NoError(t, err)

	closeDone := make(chan error, 1)
	go func() { closeDone <- proc.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err, "kill-path teardown of the whole tree must be clean")
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung: the backgrounded grandchild kept the pipes open, so the process group kill did not work")
	}
}

// TestSpawn_CloseSurfacesASelfInflictedBadExit is the boundary of the kill
// path's error suppression: a process that exited on its own with a bad
// status — no kill involved — must still have that status reported by Close.
func TestSpawn_CloseSurfacesASelfInflictedBadExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := acpexec.Spawn(ctx, exec.Command("sh", "-c", "exit 3"))
	require.NoError(t, err)

	// Drain stdout to EOF so the process is known to have exited before
	// Close runs — this must take the "already exited" branch, not the kill
	// branch.
	_, err = io.ReadAll(proc)
	require.NoError(t, err)

	err = proc.Close()
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 3, exitErr.ExitCode())
}

func TestSpawn_CtxCancellationTearsDownTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	proc, err := acpexec.Spawn(ctx, exec.Command("cat"), acpexec.WithKillGrace(200*time.Millisecond))
	require.NoError(t, err)

	cancel()

	// Once ctx is cancelled, Spawn's own watcher goroutine closes the process
	// down; Read must observe that as EOF (or a closed-pipe error) rather
	// than blocking forever.
	readDone := make(chan error, 1)
	go func() {
		_, err := proc.Read(make([]byte, 16))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		require.Error(t, err, "Read must not succeed once ctx is cancelled and the process is torn down")
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not unblock after ctx cancellation")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- proc.Close() }()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after ctx cancellation had already torn the process down")
	}
}

func TestSpawn_StderrIsForwardedToConfiguredWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	proc, err := acpexec.Spawn(ctx, exec.Command("sh", "-c", "echo oops >&2"), acpexec.WithStderr(&stderr))
	require.NoError(t, err)

	_, _ = io.ReadAll(proc) // drain stdout so Close (below) doesn't need to
	require.NoError(t, proc.Close())

	assert.Equal(t, "oops\n", stderr.String())
}

func TestSpawn_PipeSetupFailureLeavesNothingToCleanUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.Command("cat")
	// Pre-claiming Stdin makes cmd.StdinPipe() fail inside Spawn, exercising
	// the setup-failure path before any process is started.
	r, w := io.Pipe()
	defer func() { _ = r.Close(); _ = w.Close() }()
	cmd.Stdin = r

	_, err := acpexec.Spawn(ctx, cmd)
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.DeadlineExceeded))
}
