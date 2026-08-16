package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
)

type acpFileIO struct {
	transport TransportResolver
}

// NewACPFileIO reads and writes through the ACP client attached to the calling
// session, so an editor's unsaved buffers are what the agent sees.
func NewACPFileIO(transport TransportResolver) localtools.FileIO {
	return &acpFileIO{transport: transport}
}

func (a *acpFileIO) resolve(ctx context.Context) *Transport {
	if a.transport == nil {
		return nil
	}
	return a.transport(ctx)
}

func (a *acpFileIO) ReadFile(ctx context.Context, path string) ([]byte, error) {
	t := a.resolve(ctx)
	if t == nil || !t.getClientCaps().FS.ReadTextFile {
		return nil, localtools.ErrNoFilesystem
	}
	req := libacp.ReadTextFileRequest{Path: path}
	if sid := resolveACPSessionID(ctx, t); sid != "" {
		req.SessionID = sid
	}
	resp, err := t.conn.ReadTextFile(ctx, req)
	if err != nil {
		return nil, mapACPNotExist(err)
	}
	return []byte(resp.Content), nil
}

func (a *acpFileIO) WriteFile(ctx context.Context, path string, data []byte) error {
	t := a.resolve(ctx)
	if t == nil || !t.getClientCaps().FS.WriteTextFile {
		return localtools.ErrNoFilesystem
	}
	req := libacp.WriteTextFileRequest{Path: path, Content: string(data)}
	if sid := resolveACPSessionID(ctx, t); sid != "" {
		req.SessionID = sid
	}
	if _, err := t.conn.WriteTextFile(ctx, req); err != nil {
		return mapACPNotExist(err)
	}
	return nil
}

// mapACPNotExist maps a not-found *libacp.Error to os.ErrNotExist, and matches
// "not found" in an untyped downstream error's text as a compat shim.
func mapACPNotExist(err error) error {
	if mapped := libacp.AsNotExist(err); errors.Is(mapped, os.ErrNotExist) {
		return mapped
	}
	var typed *libacp.Error
	if err != nil && !errors.As(err, &typed) && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return fmt.Errorf("%w: %v", os.ErrNotExist, err)
	}
	return err
}

func NewACPCwdResolver(transport TransportResolver) func(context.Context) string {
	return func(ctx context.Context) string {
		if transport == nil {
			return ""
		}
		t := transport(ctx)
		if t == nil {
			return ""
		}
		internalID := sessionIDFromCtx(ctx)
		if internalID == "" {
			return ""
		}
		t.sessionMu.Lock()
		defer t.sessionMu.Unlock()
		for _, entry := range t.sessions {
			if entry.InternalSessionID == internalID && entry.Cwd != "" {
				return entry.Cwd
			}
		}
		return ""
	}
}

// NewServeCwdResolver returns the cwd resolver for the serve path, resolving the
// session's persisted workspace cwd from the database and re-validating it
// against the current allowlist. A refusal degrades to the default root. A nil
// tracker degrades to NoopTracker.
func NewServeCwdResolver(db libdb.DBManager, roots *vfs.Factory, tracker libtracker.ActivityTracker) func(context.Context) string {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	defaultRoot := func() string {
		if roots == nil {
			return ""
		}
		return roots.Default()
	}
	return func(ctx context.Context) string {
		stored := ""
		if db != nil {
			if internalID := sessionIDFromCtx(ctx); internalID != "" {
				stored = serveSessionCwd(ctx, db, internalID)
			}
		}
		resolved, err := vfs.ResolveSessionCwd(roots, stored, defaultRoot())
		if err != nil {
			reportErr, _, end := tracker.Start(ctx, "resolve", "session_workspace",
				"stored_cwd", stored, "default_root", defaultRoot())
			reportErr(fmt.Errorf("acpsvc: session workspace is outside the configured workspace roots; using the default root: %w", err))
			end()
			return defaultRoot()
		}
		return resolved
	}
}

// serveSessionCwd maps an internal session id to its persisted workspace cwd.
func serveSessionCwd(ctx context.Context, db libdb.DBManager, internalID string) string {
	exec := db.WithoutTransaction()
	// GetMessageIndexName is workspace-independent; serve has no workspace to pass.
	name, err := runtimetypes.NewMessageStore(exec, "").GetMessageIndexName(ctx, internalID)
	if err != nil || name == "" {
		return ""
	}
	var rec sessionCwdRecord
	if err := runtimetypes.New(exec).GetKV(ctx, acpSessionCwdKVPrefix+name, &rec); err != nil {
		return ""
	}
	return rec.Cwd
}
