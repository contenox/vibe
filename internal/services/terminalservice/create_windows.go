//go:build windows

package terminalservice

import (
	"context"
	"fmt"
	"os"
	"time"
	"unsafe"

	"github.com/contenox/contenox/errdefs"
	"github.com/contenox/contenox/internal/services/terminalstore"
	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func (s *service) Create(ctx context.Context, principal string, req CreateRequest) (*CreateResponse, error) {
	if req.CWD == "" {
		req.CWD = s.cfg.AllowedRoot
	}
	cwd, err := ResolveCwdUnderRoot(s.cfg.AllowedRoot, req.CWD)
	if err != nil {
		return nil, errdefs.BadRequest(err.Error())
	}
	shell := req.Shell
	if shell == "" {
		shell = s.cfg.DefaultShell
	}
	resolvedShell, err := resolveShell(shell)
	if err != nil {
		return nil, errdefs.BadRequest(err.Error())
	}
	shell = resolvedShell
	cols, rows := req.Cols, req.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	if s.atSessionCapacity() {
		return nil, ErrTooManySessions
	}

	conpty, input, output, err := startWindowsPseudoConsole(shell, cwd, cols, rows)
	if err != nil {
		return nil, err
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	sessRow := &terminalstore.Session{
		ID:             id,
		Principal:      principal,
		CWD:            cwd,
		Shell:          shell,
		Cols:           cols,
		Rows:           rows,
		Status:         terminalstore.SessionStatusActive,
		NodeInstanceID: s.nodeInstanceID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store().Insert(ctx, sessRow); err != nil {
		_ = conpty.shutdown(ctx)
		return nil, err
	}

	sess := &session{
		id:                    id,
		input:                 input,
		output:                output,
		pseudoConsole:         uintptr(conpty.pseudoConsole),
		processHandle:         uintptr(conpty.process),
		waitOwnsProcessHandle: true,
	}
	sess.touch()
	s.putSession(sess)

	go func(process windows.Handle) {
		defer func() { _ = windows.CloseHandle(process) }()
		_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
		_ = s.closeByID(context.Background(), id)
	}(conpty.process)

	return &CreateResponse{ID: id}, nil
}

type windowsConPTY struct {
	pseudoConsole windows.Handle
	process       windows.Handle
	input         *os.File
	output        *os.File
}

func (c windowsConPTY) shutdown(ctx context.Context) error {
	return (&session{
		input:         c.input,
		output:        c.output,
		pseudoConsole: uintptr(c.pseudoConsole),
		processHandle: uintptr(c.process),
	}).shutdown(ctx)
}

func startWindowsPseudoConsole(shell, cwd string, cols, rows int) (windowsConPTY, *os.File, *os.File, error) {
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: create conpty input pipe: %w", err)
	}
	defer closeHandleIfSet(&inRead)
	defer closeHandleIfSet(&inWrite)
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: create conpty output pipe: %w", err)
	}
	defer closeHandleIfSet(&outRead)
	defer closeHandleIfSet(&outWrite)

	var pseudoConsole windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &pseudoConsole); err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: create conpty: %w", err)
	}
	defer func() {
		if pseudoConsole != 0 {
			windows.ClosePseudoConsole(pseudoConsole)
		}
	}()

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: conpty attributes: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(&pseudoConsole),
		unsafe.Sizeof(pseudoConsole),
	); err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: conpty attribute update: %w", err)
	}

	startupInfo := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
		},
		ProcThreadAttributeList: attributes.List(),
	}
	appName, err := windows.UTF16PtrFromString(shell)
	if err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: shell path: %w", err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{shell}))
	if err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: shell command line: %w", err)
	}
	currentDir, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: cwd: %w", err)
	}

	var procInfo windows.ProcessInformation
	err = windows.CreateProcess(
		appName,
		commandLine,
		nil,
		nil,
		false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		nil,
		currentDir,
		&startupInfo.StartupInfo,
		&procInfo,
	)
	if err != nil {
		return windowsConPTY{}, nil, nil, fmt.Errorf("terminalservice: conpty process start: %w", err)
	}
	_ = windows.CloseHandle(procInfo.Thread)

	_ = windows.CloseHandle(inRead)
	inRead = 0
	_ = windows.CloseHandle(outWrite)
	outWrite = 0

	input := os.NewFile(uintptr(inWrite), "conpty-input")
	inWrite = 0
	output := os.NewFile(uintptr(outRead), "conpty-output")
	outRead = 0
	conpty := windowsConPTY{pseudoConsole: pseudoConsole, process: procInfo.Process, input: input, output: output}
	pseudoConsole = 0
	return conpty, input, output, nil
}

func closeHandleIfSet(handle *windows.Handle) {
	if handle != nil && *handle != 0 {
		_ = windows.CloseHandle(*handle)
		*handle = 0
	}
}
