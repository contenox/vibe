package contenoxcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/spf13/cobra"
)

var vetCmd = &cobra.Command{
	Use:   "vet [path]",
	Short: "Validate chain and hitl-policy files before anything runs them.",
	Long: `Validate chain files and HITL policy (envelope) files.

Chains are checked with the load-time linter: handler input/output signatures,
the dataflow across every goto and on_failure edge, input_var and template
references, transition branches that can never fire, and structural defects
(duplicate task ids, unknown handlers, dangling goto targets). HITL policies
are checked for unknown fields, invalid rule shapes, tool patterns that can
never match, and timeout values — a timeout_s below -1 or longer than seven
days, a timeout_s/on_timeout on a rule whose action never waits, and an
on_timeout beside timeout_s: -1, which never expires.

A rendered envelope is vetted the same way. Its timeouts are written in
agents.toml as 'timeout' and 'on_timeout' on a grant, and refused there with the
envelope and axis named, so a defect this reports in a hitl-policy-*.json under
.generated/ is one to fix in [envelopes.<name>].

A rule carrying no timeout_s is not a rule that waits forever: it falls to this
host's approval ceiling ('contenox config set approval-ceiling <duration|never>',
seven days until you set it) and is denied there. The rule that does wait
forever carries timeout_s: -1, written in agents.toml as timeout = "never".

` + toolGrantGrammar + `

A tool pattern is matched exactly, with "*" as the only wildcard — a partial
glob like "git_*" can never match, and is reported.

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

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	// Trigger files are a beta surface: without the opt-in they stay in the
	// "skip" class.
	vo := vetOpts{triggers: betaEnabledGlobal(), contenoxDir: contenoxDir, envelopeSearchPath: policyDirs(contenoxDir)}

	var files []string
	switch {
	case len(args) == 1:
		found, err := collectVetFiles(args[0])
		if err != nil {
			return err
		}
		files = found
	default:
		dirs := []string{contenoxDir}
		if all {
			if home, err := globalContenoxDir(); err == nil && home != contenoxDir {
				dirs = append(dirs, home)
			}
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); err != nil {
				continue
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

	failed := runVetOnFiles(cmd.OutOrStdout(), files, vo)
	if failed > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "\nvet: %d of %d file(s) failed\n", failed, len(files))
		return &exitError{1}
	}
	return nil
}

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

type vetFileKind int

const (
	vetKindSkip vetFileKind = iota
	vetKindChain
	vetKindEnvelope
	vetKindTrigger
)

type vetOpts struct {
	triggers    bool
	contenoxDir string
	// envelopeSearchPath turns on the two checks that need to know where else a
	// policy of this name lives; empty leaves a single file judged on its own.
	envelopeSearchPath []string
}

func classifyVetFile(path string, data []byte, vo vetOpts) vetFileKind {
	if vo.triggers && eventtrigger.IsTriggerFile(filepath.Base(path)) {
		return vetKindTrigger
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		if strings.HasPrefix(filepath.Base(path), "hitl-policy") {
			return vetKindEnvelope
		}
		return vetKindSkip
	}
	// Keys must hold arrays, not merely exist.
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

func jsonIsArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}

func vetOneFile(path string, vo vetOpts) (bool, error, []hitlservice.PolicyDiagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return true, fmt.Errorf("cannot read %q: %w", path, err), nil
	}
	switch classifyVetFile(path, data, vo) {
	case vetKindChain:
		var chain taskengine.TaskChainDefinition
		if err := json.Unmarshal(data, &chain); err != nil {
			return true, fmt.Errorf("chain does not parse: %w", err), nil
		}
		return true, taskengine.LintChain(&chain), nil
	case vetKindEnvelope:
		// A missing or mismatched trusted-binary entry is a warning: the envelope is
		// still valid, and the runtime refuses the call.
		diags := hitlservice.TrustedBinaryDiagnostics(data)
		diags = append(diags, shadowedEnvelopeDiagnostics(path, vo.envelopeSearchPath)...)
		errs := []error{hitlservice.VetPolicy(data)}
		errs = append(errs, unservedToolErrors(data)...)
		return true, errors.Join(errs...), diags
	case vetKindTrigger:
		return true, eventtrigger.Vet(data, resolveTriggerRef(vo.contenoxDir)), nil
	default:
		return false, nil, nil
	}
}

func runVetOnFiles(out io.Writer, files []string, vo vetOpts) int {
	failed := 0
	for _, path := range files {
		vetted, err, diags := vetOneFile(path, vo)
		switch {
		case !vetted:
			fmt.Fprintf(out, "skip %s (not a chain or hitl-policy file)\n", path)
		case err != nil:
			failed++
			fmt.Fprintf(out, "FAIL %s\n%s\n", path, indentVetError(err))
		default:
			fmt.Fprintf(out, "ok   %s\n", path)
		}
		for _, d := range diags {
			fmt.Fprintf(out, "WARN %s\n%s\n", path, indentVetLines(d.String()))
		}
	}
	return failed
}

func indentVetError(err error) string {
	return indentVetLines(err.Error())
}

func indentVetLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, line := range lines {
		lines[i] = "     " + line
	}
	return strings.Join(lines, "\n")
}
