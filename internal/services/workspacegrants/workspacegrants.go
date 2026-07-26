// Package workspacegrants owns the DURABLE, hot-reloadable half of the
// workspace-root allowlist: the grants an operator adds beyond serve's
// launch-time roots, and the cross-process doorbell that tells a running
// `contenox serve` a grant changed so it reloads without a restart.
//
// # Why a durable config + a doorbell, not one or the other
//
// The workspace-root allowlist bounds what a browser client may choose as a
// session's workspace. Until this slice it was fixed at serve launch (flags,
// WORKSPACE_ROOTS, positional args), so granting a new root meant restarting
// serve — unacceptable while a maintainer is live in the UI. The fix is two
// halves that must agree:
//
//   - The GRANT is a durable config value (clikv key ConfigKey), stored in the
//     shared ~/.contenox/local.db every process already opens. It is the SOURCE
//     OF TRUTH: a grant added while serve was down is honored at its next boot,
//     and a serve that missed a live signal still converges by re-reading it.
//   - The DOORBELL is a fire-and-forget bus event (RootsChangedSubject) on the
//     same shared SQLite bus the report-routing slice already proved carries
//     events cross-process (CLI writer → serve reader). It carries the new list
//     as a self-contained payload, per this codebase's "<owner>.events.<verb>"
//     convention, but serve treats it as a NUDGE, not the truth: on the signal
//     it RE-READS the durable config and applies that. That is deliberate — a
//     bus event can be missed, delayed, reordered, or raced by two writers, and
//     trusting its payload would let serve land on a list that lost the race,
//     whereas re-reading the single durable value cannot diverge. The payload
//     stays self-contained anyway so a lightweight observer (or a debug trace)
//     can read the intent off one event without a second lookup.
//
// This package is transport-agnostic and imports neither libbus nor the vfs
// Factory: it depends only on the runtimetypes Store (via clikv) for the config
// and on a narrow Publisher interface for the doorbell, so the CLI writer, the
// REST writer, and serve's reader all reach the SAME grant semantics without
// pulling the bus or the factory into a config concern.
package workspacegrants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// ConfigKey is the clikv key the durable workspace-root grant list is stored
// under (as clikv.Prefix+ConfigKey). It is a GLOBAL config value, not
// workspace-scoped: the allowlist is a property of the host serve process, not
// of any one project workspace. The stored value is the grant paths joined by
// the OS path-list separator (filepath.ListSeparator), matching the format of
// the WORKSPACE_ROOTS env var and serve's `filepath.SplitList` read, so the two
// grant sources normalize identically.
const ConfigKey = "workspace-roots"

// RootsChangedSubject is the bus subject the reload doorbell rings, in this
// codebase's "<owner>.events.<verb>" convention (cf.
// missionservice.ReportAddedSubject = "missionservice.events.report_added"). The
// owner is the workspace-roots surface rather than a Go package name because the
// producers (CLI and REST) and the consumer (serve) live in different packages;
// the subject names the DOMAIN event, not who emits it.
const RootsChangedSubject = "workspace.events.roots_changed"

// RootsChangedEvent is the SELF-CONTAINED doorbell payload: the writer's view of
// the new root list at the moment of the change. Self-contained by the register
// of missionservice.ReportAddedEvent — a consumer can read the intent off the one
// event — but NOT authoritative, and by design only a VIEW: the REST writer
// (running inside serve) fills it with the full effective set (base ∪ grants),
// while the CLI writer, which cannot know serve's launch base, fills it with the
// durable grants alone. Neither is trusted: serve re-reads the durable config on
// the signal and applies THAT (see the package doc for why), so the payload's
// exact contents matter only to a lightweight observer, never to the reload.
type RootsChangedEvent struct {
	Roots []string `json:"roots"`
}

// Publisher is the NARROW slice of the event bus the doorbell needs — one verb,
// Publish. libbus.Messenger satisfies it. Declared here (rather than importing
// libbus) so this package depends only on the one call it makes, mirroring
// missionservice.EventPublisher.
type Publisher interface {
	Publish(ctx context.Context, subject string, data []byte) error
}

// ErrInvalidGrant wraps every refusal of a grant PATH (empty, non-existent, not
// a directory) so a REST/CLI caller can tell a bad-input 422 apart from a
// storage failure via errors.Is. The wrapped message is the teaching text an
// operator reads.
var ErrInvalidGrant = errors.New("invalid workspace-root grant")

// ReadGrants returns the durable grant list, newest-config-first order
// preserved, empties dropped. A missing key or unreadable value yields an empty
// slice — an operator who has granted nothing simply has no grants, which is not
// an error. The returned paths are the stored (cleaned, absolute) forms; the vfs
// Factory symlink-resolves and de-duplicates them again when it builds, so this
// need not.
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

// normalizeGrant validates and canonicalizes a grant path: it must be non-empty,
// resolve to an absolute path, and name an existing DIRECTORY. Unlike
// vfs.ResolveRoot (which tolerates a not-yet-created root), a grant is a
// deliberate, explicit trust decision, so it refuses a path that does not exist
// or is a file — an operator granting a root should be granting a real directory,
// and a typo caught here is far cheaper than a "root that browses nothing"
// discovered later. Returns the cleaned absolute path to store.
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

// isBroadParent refuses roots that would defeat project scoping by handing a
// session an entire home, disk, or top-level system tree: the filesystem root,
// the operator's home directory, and any single-segment absolute path (e.g.
// "/home", "/mnt", "/srv"). A real project directory is at least two segments
// deep ("/home/me/proj", "/srv/app"), so this blocks the footguns without
// blocking real layouts. It is enforced INSIDE normalizeGrant so both the REST
// mutator and the CLI `workspace add` inherit it by construction — the same place
// the control-plane refusal is applied by the callers.
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

// samePath reports whether two grant paths denote the same directory, compared
// on their cleaned absolute forms. It is the de-dup / removal key: grants are
// stored canonicalized (normalizeGrant), so a plain equality on the cleaned
// absolute path is the honest match — "already granted" and "remove this" both
// key on the same canonical form the operator sees in `workspace list`.
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

// Add validates path, appends it to the durable grant list (idempotently — a
// path already granted is not duplicated), persists the list, and returns the
// resulting grants. A validation failure (see normalizeGrant) wraps
// ErrInvalidGrant and leaves the stored list untouched.
func Add(ctx context.Context, store runtimetypes.Store, path string) ([]string, error) {
	normalized, err := normalizeGrant(path)
	if err != nil {
		return nil, err
	}
	grants := ReadGrants(ctx, store)
	for _, g := range grants {
		if samePath(g, normalized) {
			// Already granted — idempotent no-op, but still persist-normalized below
			// is unnecessary; return the current list unchanged.
			return grants, nil
		}
	}
	grants = append(grants, normalized)
	if err := writeGrants(ctx, store, grants); err != nil {
		return nil, err
	}
	return grants, nil
}

// Remove drops every grant whose canonical path matches path and persists the
// result, returning the remaining grants. Removing a path that was never granted
// is an idempotent no-op (no error): the post-condition an operator asked for —
// "this path is not granted" — already holds. path is NOT existence-checked; an
// operator must be able to revoke a grant to a directory that has since been
// deleted or renamed.
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

// PublishChanged rings the reload doorbell with the new effective root list. It
// is BEST EFFORT and swallows its own error into a returned value the caller may
// log or ignore: the durable grant is already written before this runs, so a
// missed doorbell never loses the grant — serve converges on its next boot or
// its next signal by re-reading the config. A nil publisher is a no-op, so a
// writer built without a bus (a test, an offline `workspace add` with no serve
// running) simply persists the grant and rings nothing.
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
