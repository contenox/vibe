// Package project owns a project's portable identity marker at
// <projectRoot>/.contenox/workspace.id: a stable UUID plus an optional friendly
// Name, travelling with the directory rather than the host-local grant list.
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

// MarkerName is the project's explicit marker name — "" when the root
// carries no marker or an unnamed one. Unlike DisplayName it never invents a fallback.
func MarkerName(projectRoot string) string {
	if m, ok := ReadFromProjectRoot(projectRoot); ok {
		return m.Name
	}
	return ""
}

// Register makes projectRoot a registered, named project. An empty name defaults
// to the directory's basename, but never clobbers an already-chosen one.
func Register(projectRoot, name string) (Marker, error) {
	if strings.TrimSpace(name) == "" {
		if m, ok := ReadFromProjectRoot(projectRoot); ok && m.Name != "" {
			return m, nil
		}
		name = filepath.Base(strings.TrimRight(projectRoot, string(filepath.Separator)))
	}
	return EnsureInProjectRoot(projectRoot, name)
}

// NormalizeName validates and canonicalizes a friendly project name:
// trimmed, "" means "no name given". Control characters and names over
// MaxNameLen runes are refused, since the name renders verbatim in the UI.
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

// EnsureInContenoxDir makes contenoxDir a project, creating a marker when absent
// and otherwise returning the existing one. An empty name never clears an
// existing one, and the ID is never rewritten.
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
