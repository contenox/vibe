package localtools

// filter_config.go — config schema and loading for the tool-output filter
// engine (TODO.md S8, docs/development/blueprints/pando-mining.md item 1 /
// F2-G1). Filter config files are JSON (repo convention) and are resolved with
// the precedence project-local (.contenox/filters.json) → user-global
// (~/.contenox/filters.json) → embedded defaults; a filter in a
// higher-precedence file with the same name REPLACES the lower-precedence one
// entirely (an override outranks a built-in).
//
// Loading is fail-safe end to end: an invalid filter is skipped at load (and
// recorded as a load issue for the validator), a malformed file still
// contributes its valid entries (each filter entry is decoded independently),
// and a file that cannot be parsed at all simply drops out of the chain.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// FilterConfigFile is the on-disk JSON schema of a filter config file.
//
//	{
//	  "disabled": false,                  // live kill switch for the whole engine
//	  "filters": [ { ...FilterSpec } ],   // ordered; first regex match wins
//	  "assertions": [ { ...FilterMatchAssertion } ]
//	}
type FilterConfigFile struct {
	// Disabled is the live kill switch: when the highest-precedence file that
	// sets it says true, the engine passes every output through raw. It is
	// re-read (mtime-based) on every application, so flipping it takes effect
	// without a restart.
	Disabled   *bool                  `json:"disabled,omitempty"`
	Filters    []FilterSpec           `json:"filters,omitempty"`
	Assertions []FilterMatchAssertion `json:"assertions,omitempty"`
}

// FilterSpec is one declarative filter. Matching is a regex over the FULL
// command string ("go test -json ./..."); transforms run in a FIXED order
// regardless of the order keys appear in the file:
//
//	strip ANSI → per-line substitutions → success-collapse (unless-guard) →
//	drop-list XOR allow-list → per-line length cap (rune-safe) → head/tail
//	line windows (explicit elision markers) → absolute line cap → on-empty.
//
// Declaring both "drop" and "allow" is a config error: the filter is rejected
// at load and reported by the validator.
type FilterSpec struct {
	Name    string `json:"name"`
	Command string `json:"command"` // regex over the full command string

	// StripANSI defaults to true; set false to keep escape sequences.
	StripANSI       *bool                  `json:"strip_ansi,omitempty"`
	Substitutions   []FilterSubstitution   `json:"substitutions,omitempty"`
	SuccessCollapse *FilterSuccessCollapse `json:"success_collapse,omitempty"`
	Drop            []string               `json:"drop,omitempty"`  // drop lines matching any (XOR with Allow)
	Allow           []string               `json:"allow,omitempty"` // keep only lines matching any (XOR with Drop)
	MaxLineLength   int                    `json:"max_line_length,omitempty"`
	HeadLines       int                    `json:"head_lines,omitempty"`
	TailLines       int                    `json:"tail_lines,omitempty"`
	MaxLines        int                    `json:"max_lines,omitempty"`
	OnEmpty         string                 `json:"on_empty,omitempty"`

	// Tests are inline test cases run by `contenox tools filter test` through
	// the real transform pipeline.
	Tests []FilterTestCase `json:"tests,omitempty"`
}

// FilterSubstitution is a per-line regex substitution.
type FilterSubstitution struct {
	Pattern string `json:"pattern"`
	Replace string `json:"replace"`
}

// FilterSuccessCollapse collapses the WHOLE output to Message when the command
// exited 0 — unless the output matches the Unless guard regex, in which case
// nothing is collapsed and the rest of the pipeline runs.
type FilterSuccessCollapse struct {
	Message string `json:"message"`
	Unless  string `json:"unless,omitempty"`
}

// FilterTestCase is an inline input → expected-output test for one filter.
type FilterTestCase struct {
	Name     string `json:"name,omitempty"`
	Input    string `json:"input"`
	Want     string `json:"want"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// FilterMatchAssertion asserts engine ROUTING: that a command must (or must
// not) be handled by the named filter or native parser. Exactly one of
// MustMatch / MustNotMatch is set.
type FilterMatchAssertion struct {
	Command      string `json:"command"`
	MustMatch    string `json:"must_match,omitempty"`
	MustNotMatch string `json:"must_not_match,omitempty"`
}

// FilterLoadIssue records one entry skipped at load time (fail-safe posture:
// skip and report, never abort the file).
type FilterLoadIssue struct {
	Name string
	Err  string
}

// compiledFilter is a FilterSpec with all regexes compiled and validated.
type compiledFilter struct {
	spec           FilterSpec
	command        *regexp.Regexp
	stripANSI      bool
	subs           []compiledSubstitution
	collapseUnless *regexp.Regexp // nil = collapse unconditionally on success
	drop           []*regexp.Regexp
	allow          []*regexp.Regexp
	origin         string
}

type compiledSubstitution struct {
	re      *regexp.Regexp
	replace string
}

// loadedFilterConfig is one config source after fail-safe loading.
type loadedFilterConfig struct {
	origin     string
	loadErr    string // whole-file parse failure ("" when the file decoded)
	disabled   *bool
	filters    []*compiledFilter
	assertions []FilterMatchAssertion
	issues     []FilterLoadIssue
}

// compileFilterSpec validates and compiles one filter. Any error here causes
// the filter to be SKIPPED at load (and surfaced via the validator) — an
// invalid filter never takes the engine down.
func compileFilterSpec(spec FilterSpec, origin string) (*compiledFilter, error) {
	if spec.Name == "" {
		return nil, errors.New("filter has no name")
	}
	if spec.Command == "" {
		return nil, errors.New("filter has no command regex")
	}
	if len(spec.Drop) > 0 && len(spec.Allow) > 0 {
		return nil, errors.New("drop and allow are mutually exclusive (XOR); declare only one")
	}
	if spec.MaxLineLength < 0 || spec.HeadLines < 0 || spec.TailLines < 0 || spec.MaxLines < 0 {
		return nil, errors.New("line/length caps must not be negative")
	}
	cf := &compiledFilter{spec: spec, stripANSI: spec.StripANSI == nil || *spec.StripANSI, origin: origin}
	var err error
	if cf.command, err = regexp.Compile(spec.Command); err != nil {
		return nil, fmt.Errorf("command regex: %w", err)
	}
	for _, s := range spec.Substitutions {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("substitution %q: %w", s.Pattern, err)
		}
		cf.subs = append(cf.subs, compiledSubstitution{re: re, replace: s.Replace})
	}
	if spec.SuccessCollapse != nil {
		if spec.SuccessCollapse.Message == "" {
			return nil, errors.New("success_collapse requires a message")
		}
		if u := spec.SuccessCollapse.Unless; u != "" {
			if cf.collapseUnless, err = regexp.Compile(u); err != nil {
				return nil, fmt.Errorf("success_collapse unless regex: %w", err)
			}
		}
	}
	for _, d := range spec.Drop {
		re, err := regexp.Compile(d)
		if err != nil {
			return nil, fmt.Errorf("drop regex %q: %w", d, err)
		}
		cf.drop = append(cf.drop, re)
	}
	for _, a := range spec.Allow {
		re, err := regexp.Compile(a)
		if err != nil {
			return nil, fmt.Errorf("allow regex %q: %w", a, err)
		}
		cf.allow = append(cf.allow, re)
	}
	return cf, nil
}

// loadFilterConfigBytes decodes a config file fail-safe: each filter and
// assertion entry is decoded independently so a malformed entry only skips
// itself, and the file still contributes every valid entry. It never returns
// an error — failures are recorded in the result.
func loadFilterConfigBytes(data []byte, origin string) *loadedFilterConfig {
	out := &loadedFilterConfig{origin: origin}
	var raw struct {
		Disabled   *bool             `json:"disabled"`
		Filters    []json.RawMessage `json:"filters"`
		Assertions []json.RawMessage `json:"assertions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		out.loadErr = fmt.Sprintf("config file is not valid JSON: %v", err)
		return out
	}
	out.disabled = raw.Disabled
	for i, msg := range raw.Filters {
		var spec FilterSpec
		if err := json.Unmarshal(msg, &spec); err != nil {
			out.issues = append(out.issues, FilterLoadIssue{Name: fmt.Sprintf("filters[%d]", i), Err: err.Error()})
			continue
		}
		cf, err := compileFilterSpec(spec, origin)
		if err != nil {
			name := spec.Name
			if name == "" {
				name = fmt.Sprintf("filters[%d]", i)
			}
			out.issues = append(out.issues, FilterLoadIssue{Name: name, Err: err.Error()})
			continue
		}
		out.filters = append(out.filters, cf)
	}
	for i, msg := range raw.Assertions {
		var a FilterMatchAssertion
		if err := json.Unmarshal(msg, &a); err != nil {
			out.issues = append(out.issues, FilterLoadIssue{Name: fmt.Sprintf("assertions[%d]", i), Err: err.Error()})
			continue
		}
		if a.Command == "" || (a.MustMatch == "") == (a.MustNotMatch == "") {
			out.issues = append(out.issues, FilterLoadIssue{
				Name: fmt.Sprintf("assertions[%d]", i),
				Err:  "assertion needs a command and exactly one of must_match / must_not_match",
			})
			continue
		}
		out.assertions = append(out.assertions, a)
	}
	return out
}

// builtinFilterOrigin labels the embedded default set in reports and notices.
const builtinFilterOrigin = "builtin"

// builtinFilterConfigJSON is the embedded default filter set. Deliberately
// small and conservative — every default is error-preserving: success-collapse
// only fires on exit 0 with an unless-guard for warning/error vocabulary, and
// the non-collapsed (failure) path only trims known progress noise while
// keeping a generous tail (errors cluster at the tail). `go test -json`,
// golangci-lint, and tsc are handled by the native structured parsers, not by
// declarative filters.
const builtinFilterConfigJSON = `{
  "filters": [
    {
      "name": "npm-install",
      "command": "(^|[\\s;&|])npm\\s+(install|ci|i)\\b",
      "success_collapse": {
        "message": "npm install completed successfully. (Output collapsed by filter npm-install; raw output preserved in the spool.)",
        "unless": "(?i)(\\bERR!|error|vulnerabilit|deprecated)"
      },
      "drop": [
        "^npm timing ",
        "^npm http fetch ",
        "^npm sill "
      ],
      "tail_lines": 60,
      "max_lines": 200,
      "on_empty": "(npm output empty after filtering; raw output preserved in the spool)",
      "tests": [
        {
          "name": "clean install collapses",
          "input": "added 1204 packages in 12s\n\n140 packages are looking for funding\n  run ` + "`" + `npm fund` + "`" + ` for details",
          "want": "npm install completed successfully. (Output collapsed by filter npm-install; raw output preserved in the spool.)",
          "exit_code": 0
        },
        {
          "name": "failed install keeps the error",
          "input": "npm timing idealTree Completed in 30ms\nnpm ERR! code ERESOLVE\nnpm ERR! ERESOLVE unable to resolve dependency tree",
          "want": "npm ERR! code ERESOLVE\nnpm ERR! ERESOLVE unable to resolve dependency tree",
          "exit_code": 1
        }
      ]
    },
    {
      "name": "pip-install",
      "command": "(^|[\\s;&|])(python[0-9.]*\\s+-m\\s+)?pip3?\\s+install\\b",
      "success_collapse": {
        "message": "pip install completed successfully. (Output collapsed by filter pip-install; raw output preserved in the spool.)",
        "unless": "(?i)(error|warning|incompatible|conflict)"
      },
      "drop": [
        "^\\s*(Downloading|Collecting|Using cached|Requirement already satisfied)"
      ],
      "tail_lines": 60,
      "max_lines": 200,
      "on_empty": "(pip output empty after filtering; raw output preserved in the spool)",
      "tests": [
        {
          "name": "clean install collapses",
          "input": "Collecting requests\n  Downloading requests-2.32.0-py3-none-any.whl (64 kB)\nInstalling collected packages: requests\nSuccessfully installed requests-2.32.0",
          "want": "pip install completed successfully. (Output collapsed by filter pip-install; raw output preserved in the spool.)",
          "exit_code": 0
        },
        {
          "name": "resolver error is preserved",
          "input": "Collecting left-pad\nERROR: Could not find a version that satisfies the requirement left-pad",
          "want": "ERROR: Could not find a version that satisfies the requirement left-pad",
          "exit_code": 1
        }
      ]
    }
  ],
  "assertions": [
    {"command": "npm install", "must_match": "npm-install"},
    {"command": "npm ci --no-audit", "must_match": "npm-install"},
    {"command": "pip install -r requirements.txt", "must_match": "pip-install"},
    {"command": "go test -json ./...", "must_match": "go-test-json"},
    {"command": "git status", "must_not_match": "npm-install"},
    {"command": "ls -la", "must_not_match": "pip-install"}
  ]
}`

var (
	builtinFilterOnce   sync.Once
	builtinFilterConfig *loadedFilterConfig
)

// loadBuiltinFilterConfig parses the embedded default set once. A test asserts
// it loads with zero issues; at runtime any defect degrades fail-safe like any
// other config file.
func loadBuiltinFilterConfig() *loadedFilterConfig {
	builtinFilterOnce.Do(func() {
		builtinFilterConfig = loadFilterConfigBytes([]byte(builtinFilterConfigJSON), builtinFilterOrigin)
	})
	return builtinFilterConfig
}
