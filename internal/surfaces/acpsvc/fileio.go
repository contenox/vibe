package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

var osFallback = localtools.NewOSFileIO()

type acpFileIO struct {
	transport func() *Transport
}

func NewACPFileIO(transport func() *Transport) localtools.FileIO {
	return &acpFileIO{transport: transport}
}

func (a *acpFileIO) ReadFile(ctx context.Context, path string) ([]byte, error) {
	t := a.transport()
	if t == nil {
		return nil, errors.New("acpsvc: no active ACP transport")
	}
	if !t.getClientCaps().FS.ReadTextFile {
		return osFallback.ReadFile(ctx, path)
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
	t := a.transport()
	if t == nil {
		return errors.New("acpsvc: no active ACP transport")
	}
	if !t.getClientCaps().FS.WriteTextFile {
		return osFallback.WriteFile(ctx, path, data)
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

// mapACPNotExist maps a *libacp.Error carrying a not-found code to
// os.ErrNotExist. It also matches "not found" in an untyped downstream
// error's text as a compat shim, scoped to non-*libacp.Error values only —
// a typed error's classification stays libacp's alone.
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

func NewACPCwdResolver(transport func() *Transport) func(context.Context) string {
	return func(ctx context.Context) string {
		t := transport()
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

// NewServeCwdResolver returns the cwd resolver for the serve path, where one
// shared local_fs tool serves many per-connection transports, so it resolves
// the session's persisted workspace cwd from the database instead of closing
// over one transport.
//
// The stored cwd is re-validated against the current allowlist (via
// vfs.ResolveSessionCwd, the same procedure session/load and fleet dispatch
// use) rather than trusted, since roots can be reconfigured after the record
// was written. A refusal degrades to the default root rather than
// propagating — there is no live request to refuse, only a stale or foreign
// session record — but the degradation is reported through tracker so it
// isn't silent to the operator. Nil tracker degrades to NoopTracker.
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

// serveSessionCwd maps an internal session id to its persisted workspace cwd:
// message_indices.name is the ACP session id, under which persistSessionCwd
// stores the cwd in the KV store.
func serveSessionCwd(ctx context.Context, db libdb.DBManager, internalID string) string {
	exec := db.WithoutTransaction()
	var name string
	if err := exec.QueryRowContext(ctx,
		`SELECT name FROM message_indices WHERE id = $1`, internalID,
	).Scan(&name); err != nil || name == "" {
		return ""
	}
	var rec sessionCwdRecord
	if err := runtimetypes.New(exec).GetKV(ctx, acpSessionCwdKVPrefix+name, &rec); err != nil {
		return ""
	}
	return rec.Cwd
}
