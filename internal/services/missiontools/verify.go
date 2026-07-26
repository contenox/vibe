// verify.go is the conclusion verification gate: the check that a unit's
// CLAIMED deliverables actually exist before its "done, see file" report is
// filed as a result. A model can hallucinate an artifact as easily as a
// sentence, and a result report naming a file that was never written is worse
// than no report — it reads as success to the operator (and to the next
// mission a hand-off feeds) precisely because it is structured.
//
// The gate rides mission_report's EXISTING write path — the report always
// lands, through the same AddReport the routing/inbox machinery already hangs
// off — and only ever ANNOTATES:
//
//   - Result reports whose claimed refs/artifacts include a POSITIVELY MISSING
//     local path (os.Stat says not-exists) are downgraded result → progress —
//     the closed ReportKind set's honest word for "partial" — with a warning
//     appended to the detail naming exactly what is missing.
//   - Everything unverifiable counts as PRESENT (fail-open): URLs, prose,
//     relative paths when no workdir is known, and any stat error other than
//     not-exists (permissions, an unreadable parent). Only a positive "that
//     path does not exist" downgrades — the gate must never punish a unit for
//     the runtime's limited view.
//   - NOTHING is ever discarded: summary, detail, refs, and hand-over all land
//     verbatim; the kind changes and a warning is appended, that is all.
//
// Local paths are resolved against the unit's WORKING DIRECTORY, carried on the
// tool-call context by the transport (WithWorkdir, the same construction-time
// binding WithMissionID uses). A transport that binds no workdir simply leaves
// relative refs unverifiable — absolute paths are still checked.
package missiontools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// workdirCtxKey is the unexported context key for the unit's working directory,
// mirroring missionCtxKey: set once by the transport that built the session,
// never an argument the agent passes.
type workdirCtxKey struct{}

// WithWorkdir binds the unit's session working directory to ctx, for the
// verification gate to resolve a report's RELATIVE artifact refs against. The
// transport that constructs a dispatched unit's session calls it alongside
// WithMissionID, from the same session record (the acpsvc sessionEntry's Cwd).
// An empty dir returns ctx unchanged, so "no workdir known" stays an absent
// fact rather than a blank one.
func WithWorkdir(ctx context.Context, dir string) context.Context {
	if strings.TrimSpace(dir) == "" {
		return ctx
	}
	return context.WithValue(ctx, workdirCtxKey{}, dir)
}

// WorkdirFromContext returns the working directory bound by WithWorkdir, or ""
// when the transport bound none (relative refs are then unverifiable and count
// as present — the fail-open default).
func WorkdirFromContext(ctx context.Context) string {
	dir, _ := ctx.Value(workdirCtxKey{}).(string)
	return dir
}

// WithDowngradeRecorder wires the OPTIONAL telemetry hook the gate calls once
// per downgraded report. The composition point passes the fleet's counter bump
// (fleetservice.RecordVerificationDowngrade) — a plain func, not an interface,
// because one notification is the entire contract. It cannot be the import the
// other way around: fleetservice already imports this package for its tool-name
// constants. Nil (or never calling this option) is fully supported: the
// provider keeps a no-op, so the gate itself never nil-checks.
func WithDowngradeRecorder(record func()) Option {
	return func(p *provider) {
		if record != nil {
			p.recordDowngrade = record
		}
	}
}

// verificationWarningLead is the stable, greppable lead of the warning the gate
// appends to a downgraded report's detail — the same teaching-gate register as
// fleetservice's computeBoundLead: an operator (or a test) keys on it to tell a
// verification downgrade from anything else in a report.
const verificationWarningLead = "claimed artifacts not found"

// verificationWarning builds the teaching annotation for a downgraded report:
// it names exactly what is missing and where the gate looked, states what the
// gate did (and did not) change, and points at the remedy.
func verificationWarning(missing []string) string {
	return fmt.Sprintf(
		"%s: %s — the unit's result named artifacts that do not exist at their claimed paths. "+
			"This report was downgraded from result to progress; nothing else was changed and every claimed ref is preserved above. "+
			"If the work is genuinely done, file a new result naming the artifacts' real locations.",
		verificationWarningLead, quoteList(missing))
}

// missingArtifacts returns, of the claimed refs, exactly the ones that are
// POSITIVELY missing: refs that parse as a local path (see verifiablePath),
// resolve against workdir, and for which os.Stat reports not-exists. Order
// follows the claim order, duplicates deduplicated, so the warning reads like
// the report that earned it.
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
			// Only the positive absence downgrades. Any OTHER stat error — a
			// permission wall, an unreadable parent — is the runtime's limited
			// view, not evidence against the unit, and counts as present.
			missing = append(missing, ref)
		}
	}
	return missing
}

// verifiablePath decides whether a claimed ref is a LOCAL PATH the gate can
// honestly stat, and resolves it if so. Everything it declines is fail-open
// (counts as present):
//
//   - URLs (anything carrying a scheme separator) are not local facts.
//   - Prose (anything containing whitespace) is a description, not a path; the
//     refs schema asks for "file paths or URLs", so a single whitespace-free
//     token IS treated as the path it claims to be.
//   - Relative paths resolve against the unit's workdir; with no workdir bound
//     there is nothing honest to resolve against, so they are unverifiable.
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

// claimedRefs gathers everything a report CLAIMS as a deliverable: its Refs and
// its hand-over's Artifacts, in that order. Both carry "paths or URLs, by
// reference only" by schema, so both are the gate's business on a result.
func claimedRefs(report reportClaims) []string {
	refs := append([]string(nil), report.refs...)
	return append(refs, report.artifacts...)
}

// reportClaims is the minimal read the gate takes of a report — kept as a tiny
// value type so missingArtifacts/verifiablePath stay pure and trivially
// testable without constructing missionservice rows.
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
