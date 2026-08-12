package missiontools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type workdirCtxKey struct{}

// WithWorkdir binds the unit's session working directory to ctx for resolving a report's relative artifact refs; an empty dir returns ctx unchanged.
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

// WithDowngradeRecorder wires the optional telemetry hook the gate calls once per downgraded report; nil is fully supported since the provider keeps a no-op.
func WithDowngradeRecorder(record func()) Option {
	return func(p *provider) {
		if record != nil {
			p.recordDowngrade = record
		}
	}
}

const verificationWarningLead = "claimed artifacts not found"

func verificationWarning(missing []string) string {
	return fmt.Sprintf(
		"%s: %s — the unit's result named artifacts that do not exist at their claimed paths. "+
			"This report was downgraded from result to progress; nothing else was changed and every claimed ref is preserved above. "+
			"If the work is genuinely done, file a new result naming the artifacts' real locations.",
		verificationWarningLead, quoteList(missing))
}

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
			// Only positive absence downgrades; any other stat error counts as present.
			missing = append(missing, ref)
		}
	}
	return missing
}

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

func claimedRefs(report reportClaims) []string {
	refs := append([]string(nil), report.refs...)
	return append(refs, report.artifacts...)
}

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
