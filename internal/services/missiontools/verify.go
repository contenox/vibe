// verify.go is the conclusion verification gate: it checks that a unit's
// claimed deliverables actually exist before a "done" report is filed as a
// result. It rides mission_report's existing write path and only ever
// annotates: a result whose claimed refs/artifacts include a positively
// missing local path is downgraded to progress with a warning appended.
// Everything unverifiable (URLs, prose, relative paths with no known
// workdir, non-not-exists stat errors) counts as present — fail-open.
// Nothing is ever discarded; only the kind changes and a warning is appended.
package missiontools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// workdirCtxKey is the unexported context key for the unit's working
// directory, mirroring missionCtxKey.
type workdirCtxKey struct{}

// WithWorkdir binds the unit's session working directory to ctx, for the
// verification gate to resolve a report's relative artifact refs against.
// An empty dir returns ctx unchanged.
func WithWorkdir(ctx context.Context, dir string) context.Context {
	if strings.TrimSpace(dir) == "" {
		return ctx
	}
	return context.WithValue(ctx, workdirCtxKey{}, dir)
}

// WorkdirFromContext returns the working directory bound by WithWorkdir, or
// "" when none was bound (relative refs are then unverifiable and count as
// present).
func WorkdirFromContext(ctx context.Context) string {
	dir, _ := ctx.Value(workdirCtxKey{}).(string)
	return dir
}

// WithDowngradeRecorder wires the optional telemetry hook the gate calls
// once per downgraded report. A plain func, not an interface, since one
// notification is the whole contract. Nil (or unset) is fully supported:
// the provider keeps a no-op, so the gate itself never nil-checks.
func WithDowngradeRecorder(record func()) Option {
	return func(p *provider) {
		if record != nil {
			p.recordDowngrade = record
		}
	}
}

// verificationWarningLead is the stable, greppable lead of the warning
// appended to a downgraded report's detail.
const verificationWarningLead = "claimed artifacts not found"

// verificationWarning builds the teaching annotation for a downgraded report.
func verificationWarning(missing []string) string {
	return fmt.Sprintf(
		"%s: %s — the unit's result named artifacts that do not exist at their claimed paths. "+
			"This report was downgraded from result to progress; nothing else was changed and every claimed ref is preserved above. "+
			"If the work is genuinely done, file a new result naming the artifacts' real locations.",
		verificationWarningLead, quoteList(missing))
}

// missingArtifacts returns, of the claimed refs, exactly the ones that are
// positively missing: refs that parse as a local path (see verifiablePath)
// and for which os.Stat reports not-exists. Order follows the claim order,
// deduplicated.
func missingArtifacts(workdir string, refs []string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		path, ok := verifiablePath(workdir, ref)
		if !ok {
			continue // URL, prose, or unresolvable — fail-open, counts as present.
		}
		if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
			// Only positive absence downgrades; any other stat error (a
			// permission wall, an unreadable parent) counts as present.
			missing = append(missing, ref)
		}
	}
	return missing
}

// verifiablePath decides whether a claimed ref is a local path the gate can
// honestly stat, and resolves it if so. URLs (a scheme separator) and prose
// (containing whitespace) are not local facts; a relative path resolves
// against workdir, or is unverifiable with none bound. Everything declined
// here is fail-open (counts as present).
func verifiablePath(workdir, ref string) (string, bool) {
	if ref == "" || strings.Contains(ref, "://") {
		return "", false
	}
	if strings.IndexFunc(ref, unicode.IsSpace) >= 0 {
		return "", false
	}
	if filepath.IsAbs(ref) {
		return ref, true
	}
	if workdir == "" {
		return "", false
	}
	return filepath.Join(workdir, ref), true
}

// claimedRefs gathers everything a report claims as a deliverable: Refs then
// the hand-over's Artifacts.
func claimedRefs(report reportClaims) []string {
	refs := append([]string(nil), report.refs...)
	return append(refs, report.artifacts...)
}

// reportClaims is the minimal read the gate takes of a report, kept as a
// tiny value type so missingArtifacts/verifiablePath stay pure and testable.
type reportClaims struct {
	refs      []string
	artifacts []string
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
