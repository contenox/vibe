//go:build windows

package shellsession

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/contenox/contenox/internal/services/localtools"
	"golang.org/x/sys/windows"
)

// ErrUnsupported is returned when no PTY backend can be established; on Windows the backend is ConPTY (Windows 10 1809+), so this is reached only when that API is unavailable.
var ErrUnsupported = errors.New("shellsession: shell sessions are not supported on this platform")

var peekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

const pollInterval = 8 * time.Millisecond

type ptySession struct {
	pseudoConsole windows.Handle
	process       windows.Handle
	job           windows.Handle
	input         *os.File
	output        *os.File

	readMu sync.Mutex
	closed atomic.Bool
	once   sync.Once
}

func startPTY(spec spawnSpec) (*ptySession, error) {
	shell := spec.shell
	if shell == "" {
		shell = defaultShell()
	}
	if strings.TrimSpace(shell) == "" {
		return nil, ErrUnsupported
	}
	rows, cols := spec.rows, spec.cols
	if rows <= 0 {
		rows = defaultRows
	}
	if cols <= 0 {
		cols = defaultCols
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("shellsession: create conpty input pipe: %w", err)
	}
	defer closeHandleIfSet(&inRead)
	defer closeHandleIfSet(&inWrite)
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("shellsession: create conpty output pipe: %w", err)
	}
	defer closeHandleIfSet(&outRead)
	defer closeHandleIfSet(&outWrite)

	var pseudoConsole windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inRead, outWrite, 0, &pseudoConsole); err != nil {
		return nil, fmt.Errorf("shellsession: create conpty: %w", err)
	}
	defer func() {
		if pseudoConsole != 0 {
			windows.ClosePseudoConsole(pseudoConsole)
		}
	}()

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("shellsession: conpty attributes: %w", err)
	}
	defer attributes.Delete()
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE takes the HPCON by value in lpValue, not a pointer; passing &pseudoConsole is silently accepted and ignored, leaving the pseudoconsole empty.
	handleWord := uintptr(pseudoConsole)
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&handleWord)),
		unsafe.Sizeof(pseudoConsole),
	); err != nil {
		return nil, fmt.Errorf("shellsession: conpty attribute update: %w", err)
	}

	startupInfo := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			// STARTF_USESTDHANDLES with all NULL handles: inheriting the runtime's own stdio would let the shell eat the NDJSON bus protocol.
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attributes.List(),
	}
	appName, err := windows.UTF16PtrFromString(shell)
	if err != nil {
		return nil, fmt.Errorf("shellsession: shell path: %w", err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{shell}))
	if err != nil {
		return nil, fmt.Errorf("shellsession: shell command line: %w", err)
	}
	var currentDir *uint16
	if spec.cwd != "" {
		if currentDir, err = windows.UTF16PtrFromString(spec.cwd); err != nil {
			return nil, fmt.Errorf("shellsession: cwd: %w", err)
		}
	}
	envBlock := environmentBlock(spec.scrub)

	var procInfo windows.ProcessInformation
	err = windows.CreateProcess(
		appName,
		commandLine,
		nil,
		nil,
		false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&envBlock[0],
		currentDir,
		&startupInfo.StartupInfo,
		&procInfo,
	)
	// Kernel reads the block during the call only; KeepAlive keeps it alive until CreateProcess returns.
	runtime.KeepAlive(envBlock)
	if err != nil {
		return nil, fmt.Errorf("shellsession: conpty process start: %w", err)
	}
	_ = windows.CloseHandle(procInfo.Thread)
	job := killOnCloseJob(procInfo.Process)

	// The child owns its ends now; ours must go so EOF propagates on exit.
	_ = windows.CloseHandle(inRead)
	inRead = 0
	_ = windows.CloseHandle(outWrite)
	outWrite = 0

	s := &ptySession{
		pseudoConsole: pseudoConsole,
		process:       procInfo.Process,
		job:           job,
		input:         os.NewFile(uintptr(inWrite), "conpty-input"),
		output:        os.NewFile(uintptr(outRead), "conpty-output"),
	}
	inWrite, outRead, pseudoConsole = 0, 0, 0
	return s, nil
}

// Read returns the next available slice of the shell's output, polling for readiness so close can interrupt it, and returns io.EOF once the shell is gone.
func (p *ptySession) Read(b []byte) (int, error) {
	for {
		if p.closed.Load() {
			return 0, io.EOF
		}
		// Polls rather than blocking in ReadFile: an anonymous pipe read cannot be cancelled, and a blocked ReadFile could not be unblocked by close.
		ready, err := pipeHasData(p.output)
		if err != nil {
			return 0, err
		}
		if !ready {
			time.Sleep(pollInterval)
			continue
		}
		p.readMu.Lock()
		if p.closed.Load() {
			p.readMu.Unlock()
			return 0, io.EOF
		}
		n, err := p.output.Read(b)
		p.readMu.Unlock()
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (p *ptySession) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return p.input.Write(b)
}

func (p *ptySession) resize(rows, cols int) error {
	if p.pseudoConsole == 0 || p.closed.Load() {
		return nil
	}
	if rows <= 0 {
		rows = defaultRows
	}
	if cols <= 0 {
		cols = defaultCols
	}
	// The child sees this as a console resize, so a full-screen program reflows.
	return windows.ResizePseudoConsole(p.pseudoConsole, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (p *ptySession) close() {
	p.once.Do(func() {
		p.closed.Store(true)
		// Waits for an in-flight Read so no handle is closed underneath it.
		p.readMu.Lock()
		defer p.readMu.Unlock()
		if p.process != 0 {
			_ = windows.TerminateProcess(p.process, 1)
		}
		if p.pseudoConsole != 0 {
			windows.ClosePseudoConsole(p.pseudoConsole)
			p.pseudoConsole = 0
		}
		if p.input != nil {
			_ = p.input.Close()
		}
		if p.output != nil {
			_ = p.output.Close()
		}
		if p.job != 0 {
			_ = windows.CloseHandle(p.job)
			p.job = 0
		}
	})
}

func (p *ptySession) wait() {
	if p.process == 0 {
		return
	}
	_, _ = windows.WaitForSingleObject(p.process, windows.INFINITE)
	_ = windows.CloseHandle(p.process)
	p.process = 0
}

func pipeHasData(file *os.File) (bool, error) {
	var available uint32
	r1, _, err := peekNamedPipe.Call(file.Fd(), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
	if r1 != 0 {
		return available > 0, nil
	}
	// A broken/invalid pipe means the shell exited: reported as io.EOF, not an error.
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return false, io.EOF
	}
	return false, err
}

func environmentBlock(scrub func([]string) []string) []uint16 {
	env := os.Environ()
	if scrub != nil {
		env = scrub(env)
	}
	env = append(env, "TERM=xterm-256color")

	var buf []uint16
	for _, kv := range env {
		if kv == "" {
			continue
		}
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			// An embedded NUL cannot ride the block; dropping that one variable beats failing to open the shell.
			continue
		}
		buf = append(buf, u...)
	}
	// Terminating NUL is always appended, so &envBlock[0] in startPTY is always safe even for an empty environment.
	buf = append(buf, 0)
	return buf
}

func killOnCloseJob(process windows.Handle) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

func closeHandleIfSet(handle *windows.Handle) {
	if handle != nil && *handle != 0 {
		_ = windows.CloseHandle(*handle)
		*handle = 0
	}
}

func defaultShell() string { return localtools.DetectPlatformShell().Command }
