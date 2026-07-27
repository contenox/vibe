package acpsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
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

func mapACPNotExist(err error) error {
	// The typed classification AND the os.ErrNotExist wrapping are generic and
	// live in libacp: a *libacp.Error whose code says the resource is missing,
	// or whose code leaves the request's subject open and whose message says so.
	if mapped := libacp.AsNotExist(err); errors.Is(mapped, os.ErrNotExist) {
		return mapped
	}
	// Compat shim, deliberately NOT promoted to libacp: match "not found" in a
	// BARE Go error's text. As library behaviour this is unsafe — it would
	// swallow an agent-not-found startup failure and report a dead binary as a
	// missing file. It is kept here because a downstream that answers fs/* with
	// an untyped error still has to be understood, and fs.go's new-file-write
	// path branches on os.ErrNotExist. Scoped to errors that are NOT a
	// *libacp.Error, so a typed error's classification is libacp's alone.
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

// NewServeCwdResolver returns the cwd resolver for the serve path, where a
// single shared local_fs tool (and the shell-session manager) is consulted by
// many per-connection transports — so it cannot close over one transport the way
// the stdio path does. Instead it resolves the session's persisted workspace cwd
// from the database, keyed by the internal session id in ctx.
//
// The stored cwd is re-checked against the CURRENT allowlist rather than trusted
// because it was checked once at session/new time. Those are different claims:
// the record outlives the process that wrote it, and roots is rebuilt from the
// operator's serve arguments on every start. A session created while a root was
// configured keeps naming that root after it is dropped from the allowlist, and
// two serve instances sharing one database each read the other's session rows.
// In both cases an unchecked read would root the agent's filesystem tools — and
// its PTY — outside the envelope the running process was told to enforce. The
// transport already treats the persisted value as untrusted for exactly this
// reason on session/load and session/resume (see resolveExistingSessionCwd);
// this is the same judgement on the tool-facing side of the same record.
//
// The judgement itself is vfs.ResolveSessionCwd — the ONE decision procedure the
// session paths and fleet dispatch also resolve through — so the sentinel
// handling ("" and "/" mean "unspecified" and select the default root) is not
// re-derived here. A refusal degrades to the default root rather than
// propagating: this resolver answers "which directory does this tool call run
// in", not "may this request proceed", and it has no error channel to a caller
// that could act on one. There is no live request to refuse — only a stale or
// foreign session record — and the safe reading of it is the operator's own
// default root.
//
// The tracker is that missing channel, and the reason it is a parameter rather
// than something read off a Transport: this resolver is shared by every
// per-connection transport on the serve path, so there is no single one to ask.
// A degraded resolution is silent to the agent by design, which is exactly why
// it must not also be silent to the operator — a session quietly rooted
// somewhere other than where its record says is the kind of thing you find out
// about from instrumentation or not at all. Nil degrades to NoopTracker so the
// resolver stays constructible without one.
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
