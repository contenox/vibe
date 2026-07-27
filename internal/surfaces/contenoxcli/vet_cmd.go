package contenoxcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/spf13/cobra"
)

// vet_cmd.go is the `contenox vet` verb: the load-time chain linter
// (taskengine.LintChain) and the envelope validator (hitlservice.VetPolicy)
// run over the workspace's .contenox/ files, a named path, or — with --all —
// every config directory, so a broken chain or hitl-policy teaches at the
// terminal before anything ever runs it.

var vetCmd = &cobra.Command{
	Use:   "vet [path]",
	Short: "Validate chain and hitl-policy files before anything runs them.",
	Long: `Validate chain files and HITL policy (envelope) files.

Chains are checked with the load-time linter: handler input/output signatures,
the dataflow across every goto and on_failure edge, input_var and template
references, transition branches that can never fire, and structural defects
(duplicate task ids, unknown handlers, dangling goto targets). HITL policies
are checked for unknown fields, invalid rule shapes, tool patterns that can
never match, and timeout values.

What gets vetted:

  contenox vet                  every .json in the workspace .contenox/
  contenox vet --all            the workspace .contenox/ plus ~/.contenox/
  contenox vet chain.json       one file
  contenox vet ./mychains/      every .json under a directory

Files are classified by content: a "tasks" array is a chain, a "rules" array
(or a hitl-policy-*.json name) is an envelope; anything else is skipped.
Exits non-zero when any vetted file fails.

A file can also be reported as WARN. A warning is not a defect — the file is
valid and the runtime accepts it — it means an envelope field parses but is
not enforced as strongly as it reads, and it names what to rely on instead.
Warnings never fail the run.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runVetCmd,
}

func init() {
	vetCmd.Flags().Bool("all", false, "Vet the workspace .contenox/ AND the global ~/.contenox/ directory")
}

func runVetCmd(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")

	var files []string
	switch {
	case len(args) == 1:
		found, err := collectVetFiles(args[0])
		if err != nil {
			return err
		}
		files = found
	default:
		contenoxDir, err := ResolveContenoxDir(cmd)
		if err != nil {
			return fmt.Errorf("failed to resolve .contenox dir: %w", err)
		}
		dirs := []string{contenoxDir}
		if all {
			if home, err := globalContenoxDir(); err == nil && home != contenoxDir {
				dirs = append(dirs, home)
			}
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); err != nil {
				continue // an absent config dir has nothing to vet
			}
			found, err := collectVetFiles(dir)
			if err != nil {
				return err
			}
			files = append(files, found...)
		}
		if len(files) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Nothing to vet: no .json files found. Run 'contenox init' to scaffold .contenox/, or pass a path.")
			return nil
		}
	}

	failed := runVetOnFiles(cmd.OutOrStdout(), files)
	if failed > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "\nvet: %d of %d file(s) failed\n", failed, len(files))
		return &exitError{1}
	}
	return nil
}

// collectVetFiles expands a path argument into the .json files to vet: the
// file itself, or every .json under a directory.
func collectVetFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot vet %q: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var files []string
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".json") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cannot walk %q: %w", path, err)
	}
	return files, nil
}

// vetFileKind classifies a .json file so vet knows which validator applies.
type vetFileKind int

const (
	vetKindSkip vetFileKind = iota
	vetKindChain
	vetKindEnvelope
)

// classifyVetFile decides by content first (a "tasks" array is a chain, a
// "rules" array is an envelope) and falls back to the hitl-policy-* filename
// convention, so a policy with an empty rules list still gets vetted.
func classifyVetFile(path string, data []byte) vetFileKind {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		// Not a JSON object: let the name decide which validator reports the
		// parse defect; anything unrecognized is skipped, not failed.
		if strings.HasPrefix(filepath.Base(path), "hitl-policy") {
			return vetKindEnvelope
		}
		return vetKindSkip
	}
	// Keys must hold ARRAYS, not merely exist: a tokenizer vocab.json maps
	// every token string to a number, and "tasks"/"rules" are common enough
	// to be tokens — presence alone misclassified those files as chains.
	if raw, ok := probe["tasks"]; ok && jsonIsArray(raw) {
		return vetKindChain
	}
	if raw, ok := probe["rules"]; ok && jsonIsArray(raw) {
		return vetKindEnvelope
	}
	if _, ok := probe["default_action"]; ok {
		return vetKindEnvelope
	}
	if strings.HasPrefix(filepath.Base(path), "hitl-policy") {
		return vetKindEnvelope
	}
	return vetKindSkip
}

// jsonIsArray reports whether a raw JSON value is an array.
func jsonIsArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}

// vetOneFile returns nil for a passing file, the teaching error otherwise.
// The bool reports whether the file was actually vetted (false = skipped).
//
// The third return is the file's DIAGNOSTICS: fields that parsed, validate, and
// still do less than they read like (hitlservice.PolicyDiagnostics). They are not
// defects — a warned file loads and governs normally — so they never influence
// the verdict or the exit code. They exist because an envelope is a security
// document read by an operator who will never open the Go source that records the
// caveat, and "it parses" must not be mistaken for "it is enforced".
func vetOneFile(path string) (bool, error, []hitlservice.PolicyDiagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return true, fmt.Errorf("cannot read %q: %w", path, err), nil
	}
	switch classifyVetFile(path, data) {
	case vetKindChain:
		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(data, &chain); err != nil {
			return true, fmt.Errorf("chain does not parse: %w", err), nil
		}
		return true, taskengine.LintChain(&chain), nil
	case vetKindEnvelope:
		return true, hitlservice.VetPolicy(data), hitlservice.PolicyDiagnostics(data)
	default:
		return false, nil, nil
	}
}

// runVetOnFiles vets each file, reporting per-file verdicts to out, and
// returns how many failed.
//
// A file can be both "ok" and warned: the verdict answers "would the runtime
// accept this?", the warnings answer "does it do what you think?". Both are
// printed, and only the verdict counts toward the failure tally — a WARN that
// failed the run would pressure an operator into deleting the field rather than
// understanding it.
func runVetOnFiles(out io.Writer, files []string) int {
	failed := 0
	for _, path := range files {
		vetted, err, diags := vetOneFile(path)
		switch {
		case !vetted:
			fmt.Fprintf(out, "skip %s (not a chain or hitl-policy file)\n", path)
		case err != nil:
			failed++
			fmt.Fprintf(out, "FAIL %s\n%s\n", path, indentVetError(err))
		default:
			fmt.Fprintf(out, "ok   %s\n", path)
		}
		// Warnings print for a passing file AND a failing one: a defect elsewhere
		// in the document is no reason to withhold the fact that another field is
		// not enforced.
		for _, d := range diags {
			fmt.Fprintf(out, "WARN %s\n%s\n", path, indentVetLines(d.String()))
		}
	}
	return failed
}

func indentVetError(err error) string {
	return indentVetLines(err.Error())
}

// indentVetLines is the shared indent for everything printed under a file's
// verdict line, so a warning and a defect line up the same way.
func indentVetLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, line := range lines {
		lines[i] = "     " + line
	}
	return strings.Join(lines, "\n")
}
