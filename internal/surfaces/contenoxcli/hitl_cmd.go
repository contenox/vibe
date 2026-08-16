package contenoxcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/setupcheck"
	"github.com/spf13/cobra"
)

var hitlCmd = &cobra.Command{
	Use:   "hitl",
	Short: "Work with the HITL envelope's host-specific declarations.",
	Long: `Manage the parts of a HITL policy that describe THIS machine.

Policy rules ship as presets. Trusted-binary declarations cannot: a SHA256 is a
fact about a file on one host, so shipping one would ship a false claim. This
command records them instead.

See docs/guide/trusted-binaries.md for the full workflow.`,
}

var hitlTrustCmd = &cobra.Command{
	Use:   "trust [command-or-path ...]",
	Short: "Declare, refresh, or list the binaries an allow rule may run.",
	Long: `Record which binary a command name may resolve to, and what it must hash to.

A command_prefix_allowlist entry pins a NAME; PATH decides what that name is.
Declaring the binary closes that gap: the name is resolved to an absolute real
path at evaluation time, and refused unless the path and its SHA256 match what
this file declares.

Each argument is resolved exactly as the evaluator resolves it — PATH lookup
for a bare name (PATHEXT on Windows), then symlinks resolved to the real file —
so what is written here is by construction what the evaluator will look up.

  contenox hitl trust go git            declare two binaries by name
  contenox hitl trust /usr/bin/make     declare one by absolute path
  contenox hitl trust --refresh         re-read every declared binary (upgrades)
  contenox hitl trust --list            show every declaration's state here
  contenox hitl trust --remove go       drop a declaration

Declaring any hash makes the pin STRICT for that policy: a command with no
declared hash is refused rather than waved through. Refusal is always the
failure mode — there is no warn-and-run.`,
	RunE: runHITLTrust,
}

func init() {
	hitlTrustCmd.Flags().String("policy", "hitl-policy-default.json", "Policy to update: a preset name resolved along the policy search path, or an explicit file path")
	hitlTrustCmd.Flags().Bool("refresh", false, "Re-read every already-declared binary and rewrite its hash — the legitimate-upgrade path")
	hitlTrustCmd.Flags().Bool("list", false, "List every declaration and its state on this host; changes nothing")
	hitlTrustCmd.Flags().Bool("remove", false, "Remove the named declarations instead of adding them")
	hitlCmd.AddCommand(hitlTrustCmd)
}

func runHITLTrust(cmd *cobra.Command, args []string) error {
	policyFlag, _ := cmd.Flags().GetString("policy")
	refresh, _ := cmd.Flags().GetBool("refresh")
	list, _ := cmd.Flags().GetBool("list")
	remove, _ := cmd.Flags().GetBool("remove")
	out := cmd.OutOrStdout()

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	path, data, err := resolveTrustPolicyFile(contenoxDir, policyFlag)
	if err != nil {
		return err
	}
	current, err := readTrustedBinaries(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	if list {
		printTrustedBinaries(out, path, current)
		return nil
	}
	if !refresh && len(args) == 0 {
		return fmt.Errorf("nothing to do: name at least one command or path, or pass --refresh or --list")
	}

	updated := &hitlservice.TrustedBinaries{
		Dirs:   append([]string(nil), current.Dirs...),
		Hashes: map[string]string{},
	}
	for k, v := range current.Hashes {
		updated.Hashes[k] = v
	}

	if remove {
		if err := removeTrustedEntries(out, updated, args); err != nil {
			return err
		}
	} else {
		if refresh {
			if err := refreshTrustedEntries(out, updated); err != nil {
				return err
			}
		}
		if err := declareTrustedEntries(out, updated, args); err != nil {
			return err
		}
	}

	if err := writeTrustedBinaries(path, data, updated); err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s (%d declared binaries)\n", path, len(updated.Hashes))
	return nil
}

func resolveTrustPolicyFile(contenoxDir, nameOrPath string) (string, []byte, error) {
	nameOrPath = strings.TrimSpace(nameOrPath)
	if nameOrPath == "" {
		return "", nil, fmt.Errorf("--policy must name a policy file")
	}
	if strings.ContainsAny(nameOrPath, `/\`) || filepath.IsAbs(nameOrPath) {
		data, err := os.ReadFile(nameOrPath)
		if err != nil {
			return "", nil, fmt.Errorf("cannot read policy %q: %w", nameOrPath, err)
		}
		return nameOrPath, data, nil
	}
	path, data, ok := readPolicyFile(policyDirs(contenoxDir), nameOrPath)
	if !ok {
		return "", nil, fmt.Errorf("policy %q not found on the search path (%s) — run 'contenox init' or pass an explicit path",
			nameOrPath, strings.Join(policyDirs(contenoxDir), ", "))
	}
	return path, data, nil
}

func readTrustedBinaries(data []byte) (*hitlservice.TrustedBinaries, error) {
	var probe struct {
		TrustedBinaries *hitlservice.TrustedBinaries `json:"trusted_binaries"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("policy does not parse: %w", err)
	}
	if probe.TrustedBinaries == nil {
		return &hitlservice.TrustedBinaries{Hashes: map[string]string{}}, nil
	}
	if probe.TrustedBinaries.Hashes == nil {
		probe.TrustedBinaries.Hashes = map[string]string{}
	}
	return probe.TrustedBinaries, nil
}

func declareTrustedEntries(out io.Writer, tb *hitlservice.TrustedBinaries, names []string) error {
	for _, name := range names {
		real, sum, err := hitlservice.ResolveTrustedBinary(name)
		if err != nil {
			return fmt.Errorf("cannot declare %q: %w", name, err)
		}
		if prev, ok := tb.Hashes[real]; ok && !strings.EqualFold(prev, sum) {
			fmt.Fprintf(out, "re-declared %s\n  was %s\n  now %s\n", real, prev, sum)
		} else if !ok {
			fmt.Fprintf(out, "declared %s\n  sha256 %s\n", real, sum)
		} else {
			fmt.Fprintf(out, "unchanged %s\n", real)
		}
		tb.Hashes[real] = sum
		coverDirForBinary(out, tb, real)
	}
	return nil
}

func coverDirForBinary(out io.Writer, tb *hitlservice.TrustedBinaries, real string) {
	if len(tb.Dirs) == 0 {
		return
	}
	dir := filepath.Dir(real)
	for _, d := range tb.Dirs {
		if pathsEquivalent(d, dir) || isUnderDir(dir, d) {
			return
		}
	}
	tb.Dirs = append(tb.Dirs, dir)
	sort.Strings(tb.Dirs)
	fmt.Fprintf(out, "  added %s to trusted_binaries.dirs so this declaration can be reached\n", dir)
}

func refreshTrustedEntries(out io.Writer, tb *hitlservice.TrustedBinaries) error {
	for _, path := range sortedTrustPaths(tb.Hashes) {
		real, sum, err := hitlservice.ResolveTrustedBinary(path)
		if err != nil {
			fmt.Fprintf(out, "kept %s (cannot re-read: %v)\n", path, err)
			continue
		}
		if real != path {
			// The declared path now resolves elsewhere: record the real one.
			fmt.Fprintf(out, "moved %s -> %s\n", path, real)
			delete(tb.Hashes, path)
		}
		if prev, ok := tb.Hashes[real]; ok && strings.EqualFold(prev, sum) {
			continue
		}
		fmt.Fprintf(out, "refreshed %s\n  sha256 %s\n", real, sum)
		tb.Hashes[real] = sum
	}
	return nil
}

func removeTrustedEntries(out io.Writer, tb *hitlservice.TrustedBinaries, names []string) error {
	for _, name := range names {
		target := name
		if real, _, err := hitlservice.ResolveTrustedBinary(name); err == nil {
			target = real
		}
		if _, ok := tb.Hashes[target]; !ok {
			fmt.Fprintf(out, "not declared: %s\n", target)
			continue
		}
		delete(tb.Hashes, target)
		fmt.Fprintf(out, "removed %s\n", target)
	}
	return nil
}

func printTrustedBinaries(out io.Writer, path string, tb *hitlservice.TrustedBinaries) {
	fmt.Fprintf(out, "%s\n", path)
	if len(tb.Dirs) == 0 {
		fmt.Fprintln(out, "  dirs: (none declared — any directory a name resolves from is accepted; the hash pin still applies)")
	} else {
		for _, d := range tb.Dirs {
			fmt.Fprintf(out, "  dir  %s\n", d)
		}
	}
	if len(tb.Hashes) == 0 {
		fmt.Fprintln(out, "  hashes: (none declared — this policy's allows are not pinned to any binary)")
		return
	}
	bad := map[string]hitlservice.TrustedBinaryStatus{}
	for _, s := range hitlservice.CheckTrustedBinaries(tb) {
		bad[s.Path] = s
	}
	for _, p := range sortedTrustPaths(tb.Hashes) {
		if s, ok := bad[p]; ok {
			fmt.Fprintf(out, "  BAD  %s\n", s.String())
			continue
		}
		fmt.Fprintf(out, "  ok   %s\n       sha256 %s\n", p, tb.Hashes[p])
	}
}

// TrustBinariesRefreshCommand is the verb doctor points at when a declaration
// stopped matching this host.
const TrustBinariesRefreshCommand = "contenox hitl trust --refresh"

func trustedBinaryDrift(dirs []string) []setupcheck.TrustedBinaryDrift {
	var out []setupcheck.TrustedBinaryDrift
	for _, p := range HITLPolicyPresets {
		path, raw, ok := readPolicyFile(dirs, p.Name)
		if !ok {
			continue
		}
		diags := hitlservice.TrustedBinaryDiagnostics(raw)
		if len(diags) == 0 {
			continue
		}
		findings := make([]string, 0, len(diags))
		for _, d := range diags {
			findings = append(findings, d.Message)
		}
		out = append(out, setupcheck.TrustedBinaryDrift{Path: path, Findings: findings})
	}
	return out
}

func sortedTrustPaths(hashes map[string]string) []string {
	out := make([]string, 0, len(hashes))
	for k := range hashes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pathsEquivalent(a, b string) bool {
	return normalizeTrustPath(a) == normalizeTrustPath(b)
}

func isUnderDir(path, dir string) bool {
	prefix := normalizeTrustPath(dir)
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(normalizeTrustPath(path), prefix)
}

func normalizeTrustPath(p string) string {
	p = filepath.Clean(p)
	if filepath.Separator == '\\' {
		return strings.ToLower(p)
	}
	return p
}

func writeTrustedBinaries(path string, data []byte, tb *hitlservice.TrustedBinaries) error {
	if len(tb.Dirs) == 0 {
		tb.Dirs = nil
	}
	value, err := json.MarshalIndent(tb, "  ", "  ")
	if err != nil {
		return fmt.Errorf("encode trusted_binaries: %w", err)
	}
	updated, err := spliceTopLevelJSONMember(data, "trusted_binaries", value)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Never write a policy this build would refuse to load: a broken envelope
	// falls back to approve-everything.
	if err := hitlservice.VetPolicy(updated); err != nil {
		return fmt.Errorf("refusing to write %s: the result would not validate: %w", path, err)
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func spliceTopLevelJSONMember(doc []byte, key string, value []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("policy does not parse: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("policy is not a JSON object")
	}
	openEnd := dec.InputOffset()
	members := 0
	for dec.More() {
		before := dec.InputOffset()
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("policy does not parse: %w", err)
		}
		name, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("policy does not parse: %w", err)
		}
		members++
		if name != key {
			continue
		}
		start := bytes.IndexByte(doc[before:], '"')
		if start < 0 {
			return nil, fmt.Errorf("cannot locate the %q member", key)
		}
		start += int(before)
		var buf bytes.Buffer
		buf.Write(doc[:start])
		buf.WriteString(strconv.Quote(key) + ": ")
		buf.Write(value)
		buf.Write(doc[dec.InputOffset():])
		return buf.Bytes(), nil
	}
	var buf bytes.Buffer
	buf.Write(doc[:openEnd])
	buf.WriteString("\n  " + strconv.Quote(key) + ": ")
	buf.Write(value)
	if members > 0 {
		buf.WriteString(",")
	}
	buf.Write(doc[openEnd:])
	return buf.Bytes(), nil
}
