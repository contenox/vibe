package acpexec_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp/acpexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise acpexec.Spawn/Process against trivial subprocesses
// (cat, echo, sleep), validating the transport in isolation from ACP itself.

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

	// sleep ignores stdin close, so Close must escalate to Process.Kill.
	proc, err := acpexec.Spawn(ctx, exec.Command("sleep", "30"), acpexec.WithKillGrace(200*time.Millisecond))
	require.NoError(t, err)

	closeDone := make(chan error, 1)
	start := time.Now()
	go func() { closeDone <- proc.Close() }()
	select {
	case err := <-closeDone:
		assert.Less(t, time.Since(start), 10*time.Second, "Close should have killed the process well within its 30s sleep")
		// A kill-induced exit must not surface as a Close error.
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not kill a subprocess ignoring stdin closing")
	}
}

// TestSpawn_CloseKillsTheWholeProcessTree pins that Close kills the whole
// process group (e.g. an npx-wrapper's backgrounded grandchild), not just
// the direct child, so no descendant is left holding the pipes open.
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

// TestSpawn_CloseSurfacesASelfInflictedBadExit pins that a bad exit status
// from a process that exited on its own (no kill involved) still surfaces
// from Close.
func TestSpawn_CloseSurfacesASelfInflictedBadExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proc, err := acpexec.Spawn(ctx, exec.Command("sh", "-c", "exit 3"))
	require.NoError(t, err)

	// Drain to EOF first so Close takes the "already exited" branch, not kill.
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

	// Spawn's watcher goroutine tears the process down on cancellation; Read
	// must observe that rather than block forever.
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
	// Pre-claiming Stdin makes cmd.StdinPipe() fail inside Spawn.
	r, w := io.Pipe()
	defer func() { _ = r.Close(); _ = w.Close() }()
	cmd.Stdin = r

	_, err := acpexec.Spawn(ctx, cmd)
	require.Error(t, err)
	assert.False(t, errors.Is(err, context.DeadlineExceeded))
}
