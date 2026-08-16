// Package eventtrigger routes durable events to the task chains operators
// configured to react to them, declared in trigger-*.json files. The dispatcher
// keeps an NID cursor for catch-up, a firings table for dedup, and a hop guard
// against loops. Execution is pluggable via ChainRunner; this package runs no chains.
package eventtrigger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/libtracker"
)

// FilePrefix marks a file as a trigger definition (trigger-*.json).
const FilePrefix = "trigger-"

// TriggerTypeFireChain is the only trigger action type: run a named chain
// with the firing event as input.
const TriggerTypeFireChain = "fire_chain"

// Trigger binds an event type to a chain. Chain and Policy are file names
// resolved through the system-file lookup (workspace first, home fallback)
// at fire time, never absolute paths.
type Trigger struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	ListenFor   Listener `json:"listen_for"`
	Type        string   `json:"type"`
	Chain       string   `json:"chain"`
	Policy      string   `json:"policy,omitempty"`
}

// Listener names the exact event type a trigger reacts to.
type Listener struct {
	Type string `json:"type"`
}

// Vet validates a trigger document's shape. resolve, when non-nil, must
// report whether a referenced system file (chain, policy) is resolvable —
// its error is surfaced verbatim.
func Vet(data []byte, resolve func(name string) error) error {
	var t Trigger
	if err := json.Unmarshal(data, &t); err != nil {
		return fmt.Errorf("trigger does not parse: %w", err)
	}
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("trigger has no name")
	}
	if strings.TrimSpace(t.ListenFor.Type) == "" {
		return fmt.Errorf("trigger %q: listen_for.type is required (the exact event type, e.g. \"missionservice.events.report_added\")", t.Name)
	}
	if t.Type != TriggerTypeFireChain {
		return fmt.Errorf("trigger %q: unknown type %q (only %q is supported)", t.Name, t.Type, TriggerTypeFireChain)
	}
	if strings.TrimSpace(t.Chain) == "" {
		return fmt.Errorf("trigger %q: chain is required (a chain file name, e.g. \"chain-on-report.json\")", t.Name)
	}
	if resolve != nil {
		if err := resolve(t.Chain); err != nil {
			return fmt.Errorf("trigger %q: chain %q: %w", t.Name, t.Chain, err)
		}
		if t.Policy != "" {
			if err := resolve(t.Policy); err != nil {
				return fmt.Errorf("trigger %q: policy %q: %w", t.Name, t.Policy, err)
			}
		}
	}
	return nil
}

// IsTriggerFile reports whether base names a trigger definition file.
func IsTriggerFile(base string) bool {
	lower := strings.ToLower(base)
	return strings.HasPrefix(lower, FilePrefix) && strings.HasSuffix(lower, ".json")
}

// LoadResult reports one Load pass: the triggers that loaded and the files
// that did not (with the reason each was skipped).
type LoadResult struct {
	Triggers []Trigger
	Skipped  []SkippedFile
}

// SkippedFile is one trigger file Load refused, and why.
type SkippedFile struct {
	Path   string
	Reason string
}

// Load walks roots in precedence order for trigger-*.json files and returns the
// valid triggers. An earlier root shadows a later one by basename, and a
// duplicate trigger name keeps the first definition. Malformed files are skipped.
func Load(ctx context.Context, tracker libtracker.ActivityTracker, roots ...string) (LoadResult, error) {
	return LoadKept(ctx, tracker, nil, roots...)
}

// LoadKept is Load restricted to trigger names keep allows; a nil keep keeps
// every candidate. The gate `contenox`'s composition uses: without opt-in-beta
// keep refuses everything, so no trigger loads at all.
func LoadKept(ctx context.Context, tracker libtracker.ActivityTracker, keep func(string) bool, roots ...string) (LoadResult, error) {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	var result LoadResult
	skip := func(path, reason string) {
		result.Skipped = append(result.Skipped, SkippedFile{Path: path, Reason: reason})
		reportErr, _, end := tracker.Start(ctx, "load", "event_trigger",
			"trigger_path", path,
			"remedy", "fix the file (see 'contenox vet') — a skipped trigger never fires")
		reportErr(fmt.Errorf("event trigger file skipped: %s", reason))
		end()
	}

	claimedBase := map[string]bool{}
	claimedName := map[string]bool{}
	seenRoot := map[string]bool{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return result, fmt.Errorf("eventtrigger: resolve root %q: %w", root, err)
		}
		if seenRoot[abs] {
			continue
		}
		seenRoot[abs] = true
		// Discovery reads the operator's directories; it does not create them.
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return result, fmt.Errorf("eventtrigger: read root %q: %w", abs, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !IsTriggerFile(entry.Name()) {
				continue
			}
			if claimedBase[entry.Name()] {
				// An earlier (higher-precedence) root already provides this file.
				continue
			}
			claimedBase[entry.Name()] = true
			path := filepath.Join(abs, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				skip(path, fmt.Sprintf("cannot read: %v", err))
				continue
			}
			if err := Vet(data, nil); err != nil {
				skip(path, err.Error())
				continue
			}
			var t Trigger
			if err := json.Unmarshal(data, &t); err != nil {
				skip(path, fmt.Sprintf("does not parse: %v", err))
				continue
			}
			if keep != nil && !keep(t.Name) {
				continue
			}
			if claimedName[t.Name] {
				skip(path, fmt.Sprintf("duplicate trigger name %q (an earlier file already defines it)", t.Name))
				continue
			}
			claimedName[t.Name] = true
			result.Triggers = append(result.Triggers, t)
		}
	}
	return result, nil
}
