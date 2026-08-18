package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

// hitlPolicyFlag names the envelope a surface runs under, or a policy file to
// use verbatim.
const hitlPolicyFlag = "hitl-policy"

// chatProfileEnvelope is the envelope behind the evaluator's own fallback
// policy name, which the CLI surfaces reach when nothing narrower is set.
const chatProfileEnvelope = "default"

// profilePolicy is the envelope one surface resolved to: the filename every
// consumer looks up, plus the directory an explicitly named path contributes to
// the front of the search path.
type profilePolicy struct {
	Name string
	Dir  string
}

// source is the search path this policy resolves over. A named path sits ahead
// of everything, so `--hitl-policy ./x.json` is honoured verbatim rather than
// merely preferred.
func (p profilePolicy) source(contenoxDir string) hitlservice.PolicySource {
	dirs := policyDirs(contenoxDir)
	if p.Dir != "" {
		dirs = append([]string{p.Dir}, dirs...)
	}
	return hitlservice.NewFSPolicySource(dirs...)
}

func registerHITLPolicyFlag(c *cobra.Command) {
	c.Flags().String(hitlPolicyFlag, "",
		"Envelope this surface runs under: a name from [envelopes] in agents.toml, or a path to a hitl-policy JSON file used verbatim.")
}

// resolveProfilePolicy applies the three-way order the design fixes: a named
// argument wins, then an operator's own file on the search path, then the
// envelope this build transpiles into .generated. Only the first case can hard
// error on a missing file — the operator named that exact one.
func resolveProfilePolicy(ctx context.Context, cmd *cobra.Command, contenoxDir, envelope string, tracker libtracker.ActivityTracker) (profilePolicy, error) {
	named := ""
	if cmd != nil && cmd.Flags().Lookup(hitlPolicyFlag) != nil {
		named, _ = cmd.Flags().GetString(hitlPolicyFlag)
	}
	named = strings.TrimSpace(named)
	if isPolicyPath(named) {
		path, err := filepath.Abs(named)
		if err != nil {
			return profilePolicy{}, fmt.Errorf("--%s %q: %w", hitlPolicyFlag, named, err)
		}
		if _, err := os.Stat(path); err != nil {
			return profilePolicy{}, fmt.Errorf("--%s %q: %w", hitlPolicyFlag, named, err)
		}
		return profilePolicy{Name: filepath.Base(path), Dir: filepath.Dir(path)}, nil
	}
	if named != "" {
		name, ok := agentdecl.EnvelopeName(named)
		if !ok {
			return profilePolicy{}, fmt.Errorf("--%s %q is neither an envelope name nor a path to a policy file", hitlPolicyFlag, named)
		}
		envelope = name
	}
	pol := profilePolicy{Name: agentdecl.EnvelopePolicyFile(envelope)}
	return pol, ensureProfilePolicy(ctx, contenoxDir, envelope, named != "", tracker)
}

// isPolicyPath separates the two things --hitl-policy accepts. A bare name
// never carries a separator, so anything that does is a path the operator meant
// literally.
func isPolicyPath(value string) bool {
	if value == "" {
		return false
	}
	return strings.ContainsAny(value, `/\`) || value == "." || value == ".."
}

// ensureProfilePolicy makes the profile's envelope resolvable before a surface
// loads it, mirroring ensureProfileChain: render what agents.toml declares into
// .generated, leave an operator's own copy alone — the search path already puts
// it first — and hard error only when nothing at all resolves. A render failure
// with a copy already on the path is reported and survived: a stale envelope
// still gates, an absent one does not.
func ensureProfilePolicy(ctx context.Context, contenoxDir, envelope string, named bool, tracker libtracker.ActivityTracker) error {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	file := agentdecl.EnvelopePolicyFile(envelope)

	reportErr, reportChange, end := tracker.Start(ctx, "ensure", "profile_policy", "envelope", envelope, "named", named)
	defer end()

	renderErr := renderProfilePolicy(contenoxDir, envelope)
	if renderErr == nil {
		reportChange(contenoxDir, map[string]any{"rendered": file})
	}
	if _, _, ok := readPolicyFile(policyDirs(contenoxDir), file); ok {
		if renderErr != nil && !errors.Is(renderErr, agentdecl.ErrNoEnvelope) {
			// Reported, not fatal: the copy on the path is what gates the run.
			reportErr(renderErr)
		}
		return nil
	}
	err := fmt.Errorf("no envelope %q resolves: %s is on none of %s, and %s declares no [%s.%s]",
		envelope, file, strings.Join(policyDirs(contenoxDir), ", "), agentdecl.ConfigFilename, agentdecl.EnvelopeSection, envelope)
	if renderErr != nil && !errors.Is(renderErr, agentdecl.ErrNoEnvelope) {
		err = fmt.Errorf("render envelope %q: %w", envelope, renderErr)
	}
	reportErr(err)
	return err
}

// knownPolicyNames is what /policy offers: every envelope agents.toml declares,
// plus the preset copies still on the search path. A preset this build no
// longer writes is offered only where one is actually readable, so the list
// names files that resolve rather than files that once shipped.
func knownPolicyNames(contenoxDir string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(file string) {
		if seen[file] {
			return
		}
		seen[file] = true
		names = append(names, file)
	}
	if cfg, err := loadEnvelopeConfig(contenoxDir); err == nil {
		for _, name := range cfg.EnvelopeNames() {
			add(agentdecl.EnvelopePolicyFile(name))
		}
	}
	dirs := policyDirs(contenoxDir)
	for _, file := range embeddedPolicyNames() {
		if _, _, ok := readPolicyFile(dirs, file); ok {
			add(file)
		}
	}
	return names
}

func renderProfilePolicy(contenoxDir, envelope string) error {
	cfg, err := loadEnvelopeConfig(contenoxDir)
	if err != nil {
		return err
	}
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	_, _, err = agentdecl.SyncEnvelopePolicy(cfg, envelope, generated, agentdecl.ConfigFilename)
	return err
}

// envelopePolicyNames lists the filenames the declared envelopes render to.
func envelopePolicyNames(contenoxDir string) []string {
	cfg, err := loadEnvelopeConfig(contenoxDir)
	if err != nil {
		return nil
	}
	names := cfg.EnvelopeNames()
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, agentdecl.EnvelopePolicyFile(name))
	}
	return out
}

func loadEnvelopeConfig(contenoxDir string) (agentdecl.Config, error) {
	homeDir, err := globalContenoxDir()
	if err != nil {
		return agentdecl.Config{}, err
	}
	return agentdecl.Load(homeDir, contenoxDir)
}

// syncEnvelopePolicies renders every declared envelope into contenoxDir's
// .generated. It is what replaced seeding hitl-policy-*.json: a name is
// resolvable because the envelope behind it was transpiled, not because a copy
// of a preset was written where an operator's own file goes. One envelope that
// refuses is collected rather than thrown, so a single bad section does not
// cost the rest their render.
func syncEnvelopePolicies(contenoxDir string) (rendered []string, err error) {
	if contenoxDir == "" {
		return nil, nil
	}
	cfg, err := loadEnvelopeConfig(contenoxDir)
	if err != nil {
		return nil, err
	}
	generated := filepath.Join(contenoxDir, agentdecl.GeneratedDirName)
	var problems []error
	for _, name := range cfg.EnvelopeNames() {
		path, changed, syncErr := agentdecl.SyncEnvelopePolicy(cfg, name, generated, agentdecl.ConfigFilename)
		if syncErr != nil {
			problems = append(problems, syncErr)
			continue
		}
		if changed {
			rendered = append(rendered, path)
		}
	}
	return rendered, errors.Join(problems...)
}
