// Package workspacegrants owns the durable, hot-reloadable workspace-root
// allowlist beyond serve's launch-time roots: a durable config value
// (source of truth, read via clikv) plus a fire-and-forget bus doorbell
// (RootsChangedSubject) that tells a running serve to reload. serve treats
// the doorbell as a nudge only and always re-reads the durable config.
package workspacegrants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
)

// ConfigKey is the clikv key the durable grant list is stored under
// (clikv.Prefix+ConfigKey), a global (not workspace-scoped) config value.
// Stored as paths joined by filepath.ListSeparator, matching WORKSPACE_ROOTS.
const ConfigKey = "workspace-roots"

// RootsChangedSubject is the bus subject the reload doorbell rings on, in
// this codebase's "<owner>.events.<verb>" convention.
const RootsChangedSubject = "workspace.events.roots_changed"

// RootsChangedEvent is the doorbell payload: the writer's view of the new
// root list at the moment of change. Not authoritative — serve always
// re-reads the durable config on the signal rather than trusting this payload.
type RootsChangedEvent struct {
	Roots []string `json:"roots"`
}

// Publisher is the narrow slice of the event bus the doorbell needs.
// libbus.Messenger satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// ErrInvalidGrant wraps every refusal of a grant path (empty, non-existent,
// not a directory), so a caller can distinguish bad input from a storage
// failure via errors.Is.
var ErrInvalidGrant = errors.New("invalid workspace-root grant")

// ReadGrants returns the durable grant list, order preserved, empties
// dropped. A missing key or unreadable value yields an empty slice, not an error.
func ReadGrants(ctx context.Context, store runtimetypes.Store) []string {
	raw := clikv.Read(ctx, store, ConfigKey)
	return splitGrants(raw)
}

// splitGrants parses the stored path-list string into a trimmed, non-empty
// slice.
func splitGrants(raw string) []string {
	out := []string{}
	for _, p := range filepath.SplitList(raw) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinGrants renders a grant list back to the stored path-list string.
func joinGrants(roots []string) string {
	return strings.Join(roots, string(filepath.ListSeparator))
}

// normalizeGrant validates and canonicalizes a grant path: non-empty,
// resolves to an absolute path, and names an existing directory. Unlike
// vfs.ResolveRoot, it refuses a path that does not yet exist. Returns the
// cleaned absolute path to store.
func normalizeGrant(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a path is required", ErrInvalidGrant)
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a valid path: %v", ErrInvalidGrant, path, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %q does not exist; grant a directory that exists", ErrInvalidGrant, abs)
		}
		return "", fmt.Errorf("%w: cannot stat %q: %v", ErrInvalidGrant, abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is a file, not a directory; a workspace root must be a directory", ErrInvalidGrant, abs)
	}
	if broad, why := isBroadParent(abs); broad {
		return "", fmt.Errorf("%w: %q %s; grant a specific project directory, not a broad parent", ErrInvalidGrant, abs, why)
	}
	return abs, nil
}

// isBroadParent refuses roots that would defeat project scoping: the
// filesystem root, the operator's home directory, and any single-segment
// absolute path (e.g. "/home", "/mnt"). A real project directory is at
// least two segments deep.
func isBroadParent(abs string) (bool, string) {
	sep := string(filepath.Separator)
	if abs == filepath.Dir(abs) {
		return true, "is the filesystem root"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if h, herr := filepath.Abs(home); herr == nil && filepath.Clean(h) == abs {
			return true, "is your home directory"
		}
	}
	if trimmed := strings.Trim(abs, sep); trimmed != "" && !strings.Contains(trimmed, sep) {
		return true, "is a top-level system directory"
	}
	return false, ""
}

// samePath reports whether two grant paths denote the same directory,
// compared on their cleaned absolute forms.
func samePath(a, b string) bool {
	ac, err := filepath.Abs(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	bc, err := filepath.Abs(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	return filepath.Clean(ac) == filepath.Clean(bc)
}

// Add validates path, appends it to the durable grant list idempotently,
// persists the list, and returns the resulting grants. A validation
// failure wraps ErrInvalidGrant and leaves the stored list untouched.
func Add(ctx context.Context, store runtimetypes.Store, path string) ([]string, error) {
	normalized, err := normalizeGrant(path)
	if err != nil {
		return nil, err
	}
	grants := ReadGrants(ctx, store)
	for _, g := range grants {
		if samePath(g, normalized) {
			// Already granted: idempotent no-op.
			return grants, nil
		}
	}
	grants = append(grants, normalized)
	if err := writeGrants(ctx, store, grants); err != nil {
		return nil, err
	}
	return grants, nil
}

// Remove drops every grant whose canonical path matches path, persists the
// result, and returns the remaining grants. Removing an ungranted path is
// an idempotent no-op; path is not existence-checked, so a since-deleted
// grant can still be revoked.
func Remove(ctx context.Context, store runtimetypes.Store, path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: a path is required", ErrInvalidGrant)
	}
	grants := ReadGrants(ctx, store)
	kept := make([]string, 0, len(grants))
	for _, g := range grants {
		if samePath(g, path) {
			continue
		}
		kept = append(kept, g)
	}
	if len(kept) == len(grants) {
		return grants, nil // nothing matched — idempotent no-op
	}
	if err := writeGrants(ctx, store, kept); err != nil {
		return nil, err
	}
	return kept, nil
}

// writeGrants persists the grant list as the stored path-list string.
func writeGrants(ctx context.Context, store runtimetypes.Store, roots []string) error {
	if err := clikv.SetString(ctx, store, ConfigKey, joinGrants(roots)); err != nil {
		return fmt.Errorf("persist workspace-root grants: %w", err)
	}
	return nil
}

// PublishChanged rings the reload doorbell with the new effective root
// list. Best effort: a missed doorbell never loses the grant, since serve
// re-reads the config on its next boot or signal. A nil publisher is a no-op.
func PublishChanged(ctx context.Context, pub Publisher, roots []string) error {
	if pub == nil {
		return nil
	}
	data, err := marshalEvent(roots)
	if err != nil {
		return err
	}
	return pub.Publish(ctx, RootsChangedSubject, data)
}

// marshalEvent renders the self-contained doorbell payload. roots is copied into
// a fresh, non-nil slice so the event always carries a JSON array (never null)
// even for an empty grant set.
func marshalEvent(roots []string) ([]byte, error) {
	ev := RootsChangedEvent{Roots: append([]string{}, roots...)}
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal workspace-roots-changed event: %w", err)
	}
	return data, nil
}
