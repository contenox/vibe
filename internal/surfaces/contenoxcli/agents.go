
package contenoxcli

import _ "embed"

//go:embed agents/contenox.md
var contenoxAgentMD string

//go:embed agents/run.md
var runAgentMD string

//go:embed agents/compact.md
var compactAgentMD string

//go:embed agents/acp.md
var acpAgentMD string

//go:embed agents/acpx.md
var acpxAgentMD string

//go:embed agents/beam.md
var beamAgentMD string

//go:embed agents/fim.md
var fimAgentMD string

//go:embed agents/planner.md
var plannerAgentMD string

//go:embed agents/oracle.md
var oracleAgentMD string

//go:embed agents.toml
var agentsTOML string

var initAgentFiles = []struct {
	Name    string
	Content string
}{
	{"contenox.md", contenoxAgentMD},
	{"run.md", runAgentMD},
	{"compact.md", compactAgentMD},
	{"acp.md", acpAgentMD},
	{"acpx.md", acpxAgentMD},
	{"beam.md", beamAgentMD},
	{"fim.md", fimAgentMD},
	{"planner.md", plannerAgentMD},
	{"oracle.md", oracleAgentMD},
}
