package localtools

// filter_parsers.go — native structured parsers for the tool-output filter
// engine (S8 / pando F2-G1). Parsers are consulted BEFORE declarative
// filters; a parser that claims a command OWNS it and never falls through.
// Each parser degrades through three tiers and NEVER returns an error:
//
//	structured — full decode: keep failures + build errors + a one-line
//	             pass/fail tally, drop passing bodies,
//	grep       — the decode failed, but failure signatures are recognizable:
//	             keep only those lines plus a tally,
//	raw        — nothing recognizable: return the input unchanged.
//
// Conservatism rule shared by all parsers: when the output cannot be decoded
// AND the command failed AND no failure signature is recognizable, the answer
// is raw — never drop lines we cannot classify.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// parserTier is the degradation level a native parser resolved to.
type parserTier int

const (
	tierStructured parserTier = iota + 1
	tierGrep
	tierRaw
)

func (t parserTier) String() string {
	switch t {
	case tierStructured:
		return "structured"
	case tierGrep:
		return "grep"
	case tierRaw:
		return "raw"
	default:
		return "unknown"
	}
}

// nativeOutputParser is one structured parser: a claim predicate over the
// full command string and a parse function returning output + tier.
type nativeOutputParser struct {
	name   string
	claims func(command string) bool
	parse  func(stdout string, exitCode int) (string, parserTier)
}

// nativeOutputParsers is the fixed parser roster, consulted in order.
var nativeOutputParsers = []nativeOutputParser{
	{name: "go-test-json", claims: claimsGoTestJSON, parse: parseGoTestJSON},
	{name: "golangci-lint", claims: claimsGolangciLint, parse: parseGolangciLint},
	{name: "tsc", claims: claimsTsc, parse: parseTsc},
}

// claimingParser returns the first parser claiming the command, or nil.
func claimingParser(command string) *nativeOutputParser {
	for i := range nativeOutputParsers {
		if nativeOutputParsers[i].claims(command) {
			return &nativeOutputParsers[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// go test -json
// ---------------------------------------------------------------------------

var goTestCmdRe = regexp.MustCompile(`(^|[\s;&|(])go\s+test\b`)

func claimsGoTestJSON(command string) bool {
	return goTestCmdRe.MatchString(command) && strings.Contains(command, "-json")
}

// goTestEvent is the test2json event shape (fields we consume).
type goTestEvent struct {
	Action     string  `json:"Action"`
	Package    string  `json:"Package"`
	Test       string  `json:"Test"`
	Output     string  `json:"Output"`
	Elapsed    float64 `json:"Elapsed"`
	ImportPath string  `json:"ImportPath"` // build-output / build-fail events
}

// goTestFailSignatureRe recognizes failure lines in NON-json go test output
// (the grep degradation tier).
var goTestFailSignatureRe = regexp.MustCompile(`--- FAIL|^FAIL\b|panic:|^#\s|\.go:\d+:\d+:|build failed|^exit status`)

// parseGoTestJSON keeps failing tests' full output, build errors, and any
// non-JSON stray lines, plus a one-line tally; passing test bodies are
// dropped. This is the wedge-class transcript the whole feature exists for.
func parseGoTestJSON(stdout string, exitCode int) (string, parserTier) {
	lines := strings.Split(stdout, "\n")

	type key struct{ pkg, test string }
	testOutput := map[key][]string{}
	pkgOutput := map[string][]string{}
	var failedTests []key
	var failedPkgs []string
	failedPkgSeen := map[string]bool{}
	pkgSeen := map[string]bool{}
	var nonJSON []string
	passed, failed, skipped, jsonCount := 0, 0, 0, 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var ev goTestEvent
		if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &ev) != nil || ev.Action == "" {
			nonJSON = append(nonJSON, line)
			continue
		}
		jsonCount++
		pkg := ev.Package
		if pkg == "" {
			pkg = ev.ImportPath
		}
		if pkg != "" {
			pkgSeen[pkg] = true
		}
		switch ev.Action {
		case "output", "build-output":
			if ev.Test != "" {
				k := key{pkg, ev.Test}
				testOutput[k] = append(testOutput[k], ev.Output)
			} else {
				pkgOutput[pkg] = append(pkgOutput[pkg], ev.Output)
			}
		case "pass":
			if ev.Test != "" {
				passed++
			}
		case "skip":
			if ev.Test != "" {
				skipped++
			}
		case "fail", "build-fail":
			if ev.Test != "" {
				failed++
				failedTests = append(failedTests, key{pkg, ev.Test})
			} else if pkg != "" && !failedPkgSeen[pkg] {
				failedPkgSeen[pkg] = true
				failedPkgs = append(failedPkgs, pkg)
			}
		}
	}

	if jsonCount == 0 {
		// Grep tier: the stream is not test2json at all. Keep only lines
		// carrying a failure signature; if none exist we cannot classify
		// anything — raw.
		var kept []string
		for _, line := range lines {
			if goTestFailSignatureRe.MatchString(line) {
				kept = append(kept, line)
			}
		}
		if len(kept) == 0 {
			return stdout, tierRaw
		}
		kept = append(kept, fmt.Sprintf("go test (grep tier): kept %d failure-signature lines of %d total; passing output elided.", len(kept), len(lines)))
		return strings.Join(kept, "\n"), tierGrep
	}

	// Conservative guard: a non-zero exit with no decoded failure and no
	// stray lines means the failure is not visible in this stream (it is
	// likely on stderr, which is never filtered) — keep stdout raw rather
	// than collapse a run we cannot explain.
	if exitCode != 0 && failed == 0 && len(failedPkgs) == 0 && len(nonJSON) == 0 {
		return stdout, tierRaw
	}

	var b strings.Builder
	for _, k := range failedTests {
		for _, out := range testOutput[k] {
			b.WriteString(out)
		}
	}
	for _, pkg := range failedPkgs {
		for _, out := range pkgOutput[pkg] {
			b.WriteString(out)
		}
	}
	for _, line := range nonJSON {
		b.WriteString(line)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "go test: %d passed, %d failed, %d skipped (%d packages).", passed, failed, skipped, len(pkgSeen))
	if passed > 0 || skipped > 0 {
		b.WriteString(" (Passing test output elided by the go-test-json filter; raw output preserved in the spool.)")
	}
	return b.String(), tierStructured
}

// ---------------------------------------------------------------------------
// golangci-lint
// ---------------------------------------------------------------------------

var golangciCmdRe = regexp.MustCompile(`(^|[\s;&|(])golangci-lint\s+run\b`)

func claimsGolangciLint(command string) bool {
	return golangciCmdRe.MatchString(command)
}

// golangciIssueLineRe recognizes file:line[:col]: references — both the grep
// degradation tier's signature and the shape of text-format issues.
var golangciIssueLineRe = regexp.MustCompile(`\.go:\d+(:\d+)?:|(?i)^level=(warn|error)|^\d+ issues`)

func parseGolangciLint(stdout string, exitCode int) (string, parserTier) {
	var rep struct {
		Issues []struct {
			FromLinter string `json:"FromLinter"`
			Text       string `json:"Text"`
			Severity   string `json:"Severity"`
			Pos        struct {
				Filename string `json:"Filename"`
				Line     int    `json:"Line"`
				Column   int    `json:"Column"`
			} `json:"Pos"`
		} `json:"Issues"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err == nil && strings.TrimSpace(stdout) != "" {
		if len(rep.Issues) == 0 {
			return "golangci-lint: no issues found.", tierStructured
		}
		var b strings.Builder
		for _, is := range rep.Issues {
			fmt.Fprintf(&b, "%s:%d:%d: %s (%s)\n", is.Pos.Filename, is.Pos.Line, is.Pos.Column, is.Text, is.FromLinter)
		}
		fmt.Fprintf(&b, "golangci-lint: %d issues.", len(rep.Issues))
		return b.String(), tierStructured
	}

	// Grep tier: not JSON (text format, or a partial stream). Keep issue
	// references and summary lines only.
	lines := strings.Split(stdout, "\n")
	var kept []string
	for _, line := range lines {
		if golangciIssueLineRe.MatchString(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) > 0 {
		kept = append(kept, fmt.Sprintf("golangci-lint (grep tier): kept %d issue/summary lines of %d total.", len(kept), len(lines)))
		return strings.Join(kept, "\n"), tierGrep
	}
	if exitCode == 0 && strings.TrimSpace(stdout) == "" {
		return stdout, tierRaw // nothing to compress
	}
	return stdout, tierRaw
}

// ---------------------------------------------------------------------------
// tsc (TypeScript compiler diagnostics)
// ---------------------------------------------------------------------------

var tscCmdRe = regexp.MustCompile(`(^|[\s;&|(])(npx\s+|yarn\s+|pnpm\s+(exec\s+)?)?tsc\b`)

func claimsTsc(command string) bool {
	return tscCmdRe.MatchString(command)
}

var (
	// Classic and pretty diagnostic formats:
	//   src/a.ts(12,5): error TS2345: ...
	//   src/a.ts:12:5 - error TS2345: ...
	tscDiagRe    = regexp.MustCompile(`^.+?(\(\d+,\d+\):|:\d+:\d+ -) (error|warning) TS\d+:`)
	tscSummaryRe = regexp.MustCompile(`^Found \d+ error`)
	tscErrRefRe  = regexp.MustCompile(`\berror TS\d+\b`)
)

func parseTsc(stdout string, exitCode int) (string, parserTier) {
	clean := stripANSIEscapes(strings.ReplaceAll(stdout, "\r\n", "\n"))
	lines := strings.Split(clean, "\n")
	var kept []string
	for _, line := range lines {
		if tscDiagRe.MatchString(line) || tscSummaryRe.MatchString(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) > 0 {
		kept = append(kept, fmt.Sprintf("tsc: %d diagnostic/summary lines kept of %d total.", len(kept), len(lines)))
		return strings.Join(kept, "\n"), tierStructured
	}
	// Grep tier: no well-formed diagnostics, but error references exist.
	var refs []string
	for _, line := range lines {
		if tscErrRefRe.MatchString(line) {
			refs = append(refs, line)
		}
	}
	if len(refs) > 0 {
		refs = append(refs, fmt.Sprintf("tsc (grep tier): kept %d error-reference lines of %d total.", len(refs), len(lines)))
		return strings.Join(refs, "\n"), tierGrep
	}
	if exitCode == 0 && strings.TrimSpace(clean) != "" {
		return "tsc: success, no diagnostics. (Output elided by the tsc filter; raw output preserved in the spool.)", tierStructured
	}
	return stdout, tierRaw
}
