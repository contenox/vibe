package contenoxcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/services/missiontools"
	"github.com/contenox/contenox/internal/services/oracletools"
)

// governedToolsets are the toolsets this build owns the tool names of, so a
// name outside the list is a typo rather than an unknown capability. A toolset
// that is NOT here — an MCP server, a connected remote tool, a toolset a later
// build revives — is left alone: vet cannot enumerate what an operator
// connected, and refusing it would refuse the working case.
var governedToolsets = map[string][]string{
	localtools.LocalFSToolsName: {
		"read_file", "read_file_range", "write_file", "edit_file", "sed",
		// Evaluated but not executed: the probes accessview and agentview run,
		// plus the read verbs a revived filesystem toolset comes back to.
		"list_dir", "find_files", "grep", "stat_file", "count_stats",
	},
	localtools.LocalExecToolsName: {localtools.LocalExecToolsName},
	missiontools.ToolsProviderName: {
		missiontools.ToolNameReport,
		missiontools.ToolNamePlan,
		missiontools.ToolNameAskAttention,
		missiontools.ToolNameFinish,
		missiontools.ToolNameListMissions,
		missiontools.ToolNameAnswer,
		missiontools.ToolNameStartMission,
	},
	oracletools.ToolsProviderName: {
		oracletools.ToolNameSubmitVerdict,
		oracletools.ToolNameVerdictState,
	},
}

// unservedToolErrors reports every rule naming a tool its toolset does not
// serve. A rule that can never match is inert, and an inert rule in an envelope
// reads as a grant or a deny that is simply not there.
func unservedToolErrors(data []byte) []error {
	var p hitlservice.Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	var errs []error
	for i, r := range p.Rules {
		served, governed := governedToolsets[r.Tools]
		if !governed || r.Tool == "" || r.Tool == "*" {
			continue
		}
		found := false
		for _, name := range served {
			if name == r.Tool {
				found = true
				break
			}
		}
		if found {
			continue
		}
		known := append([]string(nil), served...)
		sort.Strings(known)
		errs = append(errs, fmt.Errorf("rule %d: %s serves no tool %q, so this rule can never match — known tools: %s",
			i, r.Tools, r.Tool, strings.Join(known, ", ")))
	}
	return errs
}

// shadowedEnvelopeDiagnostics reports a hand-written policy that shadows a
// rendered envelope of the same name. Neither file is wrong — the search path
// is doing what it promises — but the transpiled one is inert, so an operator
// editing agents.toml would see nothing change.
func shadowedEnvelopeDiagnostics(path string, dirs []string) []hitlservice.PolicyDiagnostic {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "hitl-policy-") || !strings.HasSuffix(name, ".json") {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	var out []hitlservice.PolicyDiagnostic
	for _, dir := range dirs {
		if dir == "" || filepath.Base(dir) != agentdecl.GeneratedDirName {
			continue
		}
		rendered := filepath.Join(dir, name)
		if renderedAbs, err := filepath.Abs(rendered); err != nil || renderedAbs == abs {
			continue
		}
		if _, err := os.Stat(rendered); errors.Is(err, os.ErrNotExist) {
			continue
		}
		out = append(out, hitlservice.PolicyDiagnostic{
			Field:   "policy",
			Message: fmt.Sprintf("this file shadows the rendered envelope %s — the transpiled one is never read, so editing [%s] in %s changes nothing here", rendered, agentdecl.EnvelopeSection, agentdecl.ConfigFilename),
		})
	}
	return out
}
