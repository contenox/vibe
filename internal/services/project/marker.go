// Package project owns a project's portable identity marker —
// <projectRoot>/.contenox/workspace.id — read and written the SAME way by serve,
// the CLI, and the /workspace/roots API so a "project" means one thing everywhere.
//
// The marker carries a stable UUID (the DB workspace-scoping token every session
// under the project is filed under) plus an optional friendly Name (what the Beam
// project registry shows). It is the SOURCE OF TRUTH for the name: the name
// travels WITH the directory (portable across hosts), not in the host-local grant
// list. A legacy bare-UUID marker (what `contenox init` historically wrote) is
// read as {ID: <content>, Name: ""}, so existing installs keep their token — the
// only content-reader of the file, contenoxcli.ResolveWorkspaceID, delegates here.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	// ContenoxDirName is a project's per-directory config/marker dir (like .git).
	ContenoxDirName = ".contenox"
	// MarkerFileName is the identity marker inside ContenoxDirName.
	MarkerFileName = "workspace.id"
	// MaxNameLen bounds a friendly name, in runes — a display-name limit applied
	// by NormalizeName at every boundary a name enters through.
	MaxNameLen = 120
)

// Marker is a project's portable identity, stored as JSON in the marker file.
type Marker struct {
	// ID is the stable workspace UUID — the DB scoping token. Never changes once
	// written, so existing session/message rows stay attached to the project.
	ID string `json:"id"`
	// Name is the friendly display name (optional; empty falls back to basename).
	Name string `json:"name,omitempty"`
}

func contenoxDirOf(projectRoot string) string {
	return filepath.Join(projectRoot, ContenoxDirName)
}

func markerPath(contenoxDir string) string {
	return filepath.Join(contenoxDir, MarkerFileName)
}

// parseMarker parses JSON {id,name}; a value that is not JSON with a non-empty id
// is treated as a legacy bare-UUID string (the whole trimmed content is the ID).
func parseMarker(data []byte) Marker {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return Marker{}
	}
	var m Marker
	if err := json.Unmarshal([]byte(trimmed), &m); err == nil && strings.TrimSpace(m.ID) != "" {
		return Marker{ID: strings.TrimSpace(m.ID), Name: strings.TrimSpace(m.Name)}
	}
	return Marker{ID: trimmed}
}

// ReadFromContenoxDir reads the marker from a `.contenox` dir. ok is false when
// the file is absent or unreadable.
func ReadFromContenoxDir(contenoxDir string) (Marker, bool) {
	data, err := os.ReadFile(markerPath(contenoxDir))
	if err != nil {
		return Marker{}, false
	}
	return parseMarker(data), true
}

// ReadFromProjectRoot reads the marker at <projectRoot>/.contenox/workspace.id.
func ReadFromProjectRoot(projectRoot string) (Marker, bool) {
	return ReadFromContenoxDir(contenoxDirOf(projectRoot))
}

// HasMarker reports whether projectRoot carries a project marker.
func HasMarker(projectRoot string) bool {
	_, ok := ReadFromProjectRoot(projectRoot)
	return ok
}

// DisplayName is the marker's Name, or the project root's basename when there is
// no marker or the marker carries no name — so a root is always presentable.
func DisplayName(projectRoot string) string {
	if name := MarkerName(projectRoot); name != "" {
		return name
	}
	return filepath.Base(strings.TrimRight(projectRoot, string(filepath.Separator)))
}

// MarkerName is the project's EXPLICIT marker name — "" when the root carries no
// marker or an unnamed one. Unlike DisplayName it never invents a fallback, so a
// caller (the /workspace/roots response) can tell a real named project from a
// structural root it would label by basename itself.
func MarkerName(projectRoot string) string {
	if m, ok := ReadFromProjectRoot(projectRoot); ok {
		return m.Name
	}
	return ""
}

// Register makes projectRoot a REGISTERED, named project — the operation behind
// `workspace add` and the POST /workspace/roots mutator. Registering is naming:
// an empty `name` defaults to the directory's basename (the same default
// `init --project` applies) — but only onto a fresh or unnamed marker, so
// re-registering without a name NEVER clobbers a name someone chose. An explicit
// non-empty name renames (EnsureInProjectRoot semantics).
func Register(projectRoot, name string) (Marker, error) {
	if strings.TrimSpace(name) == "" {
		if m, ok := ReadFromProjectRoot(projectRoot); ok && m.Name != "" {
			return m, nil
		}
		name = filepath.Base(strings.TrimRight(projectRoot, string(filepath.Separator)))
	}
	return EnsureInProjectRoot(projectRoot, name)
}

// NormalizeName validates and canonicalizes a friendly project name arriving
// from any boundary (the REST add body, CLI --name flags): trimmed, with ""
// meaning "no name given" (always valid). Control characters and names longer
// than MaxNameLen runes are refused — the name is rendered verbatim in pickers,
// chips, and CLI output, so it must stay a plain single-line label.
func NormalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	if n := utf8.RuneCountInString(name); n > MaxNameLen {
		return "", fmt.Errorf("project name is too long (%d characters, max %d)", n, MaxNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("project name must not contain control characters")
		}
	}
	return name, nil
}

// EnsureInContenoxDir makes contenoxDir a project: absent → write a marker with a
// fresh UUID and `name`; present → return the existing marker, with a non-empty
// `name` that differs REPLACING the stored one (an explicit name is an explicit
// rename — re-registering a project under a new name is the rename affordance).
// An empty `name` never clears an existing one, and the ID is never rewritten,
// so session/message rows stay attached. Returns the effective marker.
func EnsureInContenoxDir(contenoxDir, name string) (Marker, error) {
	name = strings.TrimSpace(name)
	if m, ok := ReadFromContenoxDir(contenoxDir); ok {
		if name != "" && m.Name != name {
			m.Name = name
			if err := writeMarker(contenoxDir, m); err != nil {
				return Marker{}, err
			}
		}
		return m, nil
	}
	if err := os.MkdirAll(contenoxDir, 0o750); err != nil {
		return Marker{}, err
	}
	m := Marker{ID: uuid.NewString(), Name: name}
	if err := writeMarker(contenoxDir, m); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// EnsureInProjectRoot is EnsureInContenoxDir for a project root.
func EnsureInProjectRoot(projectRoot, name string) (Marker, error) {
	return EnsureInContenoxDir(contenoxDirOf(projectRoot), name)
}

func writeMarker(contenoxDir string, m Marker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(contenoxDir), append(data, '\n'), 0o644)
}
