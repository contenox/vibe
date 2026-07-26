package contenoxcli

import (
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
Exits non-zero when any vetted file fails.`,
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
	if _, ok := probe["tasks"]; ok {
		return vetKindChain
	}
	if _, ok := probe["rules"]; ok {
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

// vetOneFile returns nil for a passing file, the teaching error otherwise.
// The bool reports whether the file was actually vetted (false = skipped).
func vetOneFile(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return true, fmt.Errorf("cannot read %q: %w", path, err)
	}
	switch classifyVetFile(path, data) {
	case vetKindChain:
		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(data, &chain); err != nil {
			return true, fmt.Errorf("chain does not parse: %w", err)
		}
		return true, taskengine.LintChain(&chain)
	case vetKindEnvelope:
		return true, hitlservice.VetPolicy(data)
	default:
		return false, nil
	}
}

// runVetOnFiles vets each file, reporting per-file verdicts to out, and
// returns how many failed.
func runVetOnFiles(out io.Writer, files []string) int {
	failed := 0
	for _, path := range files {
		vetted, err := vetOneFile(path)
		switch {
		case !vetted:
			fmt.Fprintf(out, "skip %s (not a chain or hitl-policy file)\n", path)
		case err != nil:
			failed++
			fmt.Fprintf(out, "FAIL %s\n%s\n", path, indentVetError(err))
		default:
			fmt.Fprintf(out, "ok   %s\n", path)
		}
	}
	return failed
}

func indentVetError(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	for i, line := range lines {
		lines[i] = "     " + line
	}
	return strings.Join(lines, "\n")
}
