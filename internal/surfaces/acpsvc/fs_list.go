package acpsvc

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/vfs"
	libacp "github.com/contenox/contenox/libacp"
)

// extMethodFSList lists ONE level of a session's workspace for a client that
// renders it — the `@`-mention picker and the workspace tree.
//
// It exists because ACP's own fs/* runs the other way: those are agent->client
// requests, so a client has no way to ask what is on the machine. This is the
// same direction and the same shape as extMethodTerminalRun.
//
// ⚠ Read-only, and a LISTING only: no contents, no stat beyond directory-ness.
// Reading a file is the agent's job through its own tools, where HITL policy
// applies.
const extMethodFSList = "_contenox/fs/list"

// fsListMaxEntries caps one directory's answer. A generated or vendored
// directory can hold tens of thousands of entries, and a picker cannot show
// them; the cap keeps one keystroke from turning into a megabyte on the wire.
const fsListMaxEntries = 500

type fsListParams struct {
	SessionID string `json:"sessionId"`
	// Path is relative to the session's workspace root. Empty (or ".") is the
	// root itself. Absolute paths and ".." escapes are refused, not clamped.
	Path string `json:"path,omitempty"`
}

type fsListEntry struct {
	Name string `json:"name"`
	// Path is root-relative and slash-separated on every platform, so a client
	// can hand it straight back as the next Path without knowing the host's
	// separator.
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

type fsListResult struct {
	Path    string        `json:"path"`
	Entries []fsListEntry `json:"entries"`
	// Truncated marks a directory bigger than fsListMaxEntries. The client says
	// so rather than implying the listing is complete.
	Truncated bool `json:"truncated,omitempty"`
}

// handleFSList answers a `_contenox/fs/list` request.
func (t *Transport) handleFSList(_ context.Context, params json.RawMessage) (json.RawMessage, *libacp.Error) {
	var p fsListParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "invalid params: %v", err)
		}
	}
	sid := libacp.SessionID(p.SessionID)
	if sid == "" {
		return nil, libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
	}
	entry, ok := t.sessionFor(sid)
	if !ok {
		return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "unknown session %q", sid)
	}
	root := entry.Cwd
	if strings.TrimSpace(root) == "" {
		return nil, libacp.NewError(libacp.ErrMethodNotFound, "this session has no workspace to list")
	}

	rel, err := cleanListPath(p.Path)
	if err != nil {
		return nil, libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	// The containment check is vfs's, not this file's: it resolves symlinks
	// before comparing, so a link inside the workspace pointing out of it is
	// refused rather than followed.
	abs, cErr := vfs.Contain(root, filepath.Join(root, filepath.FromSlash(rel)))
	if cErr != nil {
		return nil, libacp.NewError(libacp.ErrInvalidParams, "that path is outside the session's workspace")
	}

	dir, rErr := os.ReadDir(abs)
	if rErr != nil {
		if os.IsNotExist(rErr) {
			return nil, libacp.NewErrorf(libacp.ErrInvalidParams, "no such directory %q", rel)
		}
		return nil, libacp.InternalError(rErr.Error())
	}

	// The SAME predicate the agent's list_dir applies — the workspace
	// .gitignore plus default skip-directories. A picker that offered
	// node_modules when the agent's own listing hides it would be showing a
	// different workspace than the one being worked in.
	skip := localtools.BrowseFilter(root)
	entries := make([]fsListEntry, 0, len(dir))
	truncated := false
	for _, d := range dir {
		name := d.Name()
		childRel := path.Join(rel, name)
		if childRel == "." {
			continue
		}
		if skip(childRel, name, d.IsDir()) {
			continue
		}
		if len(entries) >= fsListMaxEntries {
			truncated = true
			break
		}
		entries = append(entries, fsListEntry{Name: name, Path: childRel, IsDir: d.IsDir()})
	}
	// Directories first, then case-insensitive by name: the order a person
	// scanning a tree expects, decided here so every client agrees.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	out, mErr := json.Marshal(fsListResult{Path: rel, Entries: entries, Truncated: truncated})
	if mErr != nil {
		return nil, libacp.InternalError(mErr.Error())
	}
	return out, nil
}

// cleanListPath normalises a client-supplied path to a root-relative,
// slash-separated one, or reports why it cannot be used.
//
// Refuses rather than clamps: a caller that asked for "../.." meant something,
// and quietly answering with the workspace root instead would let a browsing UI
// believe it had escaped when it had not.
func cleanListPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" || p == "." {
		return ".", nil
	}
	p = filepath.ToSlash(p)
	if strings.HasPrefix(p, "/") {
		return "", errListPath("path must be relative to the workspace root")
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errListPath("path must not leave the workspace root")
	}
	return cleaned, nil
}

type errListPath string

func (e errListPath) Error() string { return string(e) }
