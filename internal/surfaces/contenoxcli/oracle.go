package contenoxcli

import (
	"context"
	"fmt"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

const (
	oracleAgentName             = "oracle"
	oracleDefaultPolicyName     = "hitl-policy-oracle.json"
	configKeyOracleChain        = "default-oracle-chain"
	configKeyOraclePolicy       = "default-oracle-policy"
	configKeyOracleApprovesCall = "oracle-approves-tool-calls"

	flagOracleChain    = "oracle"
	flagOraclePolicy   = "oracle-policy"
	flagOracleApproves = "oracle-approves-tool-calls"
)

func registerOracleFlags(c *cobra.Command) {
	c.Flags().String(flagOracleChain, "", "Chain that adjudicates a subagent's asks, overriding `config set default-oracle-chain`. \"off\" disables it for this run.")
	c.Flags().String(flagOraclePolicy, "", "Envelope the oracle chain runs under, overriding `config set default-oracle-policy`.")
	c.Flags().Bool(flagOracleApproves, false, "Let the oracle rule on gated TOOL CALLS, not just questions, overriding `config set oracle-approves-tool-calls`.")
}

type oracleConfig struct {
	chain    string
	policy   string
	approves bool
}

func (c oracleConfig) enabled() bool { return strings.TrimSpace(c.chain) != "" }

func readOracleConfig(ctx context.Context, store runtimetypes.Store) oracleConfig {
	c := oracleConfig{
		chain:    strings.TrimSpace(clikv.Read(ctx, store, configKeyOracleChain)),
		policy:   strings.TrimSpace(clikv.Read(ctx, store, configKeyOraclePolicy)),
		approves: strings.EqualFold(strings.TrimSpace(clikv.Read(ctx, store, configKeyOracleApprovesCall)), "true"),
	}
	if strings.EqualFold(c.chain, "off") || strings.EqualFold(c.chain, "none") {
		c.chain = ""
	}
	if c.policy == "" {
		c.policy = oracleDefaultPolicyName
	}
	return c
}

// resolveOracleConfig reads the stored defaults and lets args override them.
func resolveOracleConfig(ctx context.Context, store runtimetypes.Store, cmd *cobra.Command) oracleConfig {
	c := readOracleConfig(ctx, store)
	flags := cmd.Flags()
	if flags.Changed(flagOracleChain) {
		v, _ := flags.GetString(flagOracleChain)
		c.chain = strings.TrimSpace(v)
		if strings.EqualFold(c.chain, "off") || strings.EqualFold(c.chain, "none") {
			c.chain = ""
		}
	}
	if flags.Changed(flagOraclePolicy) {
		if v, _ := flags.GetString(flagOraclePolicy); strings.TrimSpace(v) != "" {
			c.policy = strings.TrimSpace(v)
		}
	}
	if flags.Changed(flagOracleApproves) {
		c.approves, _ = flags.GetBool(flagOracleApproves)
	}
	return c
}

// oracleChainCandidates renders one configured value as the filenames it could
// mean, so the key takes an agent name as readily as a filename.
func oracleChainCandidates(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	out := []string{name}
	if !strings.HasSuffix(name, ".json") {
		out = append(out, name+".json")
		if !strings.HasPrefix(name, "chain-") {
			out = append(out, "chain-"+name+".json")
		}
	}
	return out
}

func loadOracleChain(contenoxDir string, c oracleConfig) (*taskengine.TaskChainDefinition, string, error) {
	candidates := oracleChainCandidates(c.chain)
	for _, name := range candidates {
		path, err := lookupSystemFile(contenoxDir, name)
		if err != nil {
			continue
		}
		chain, err := loadChainFromFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("oracle: load %s: %w", path, err)
		}
		return chain, path, nil
	}
	return nil, "", fmt.Errorf("oracle: %q names no chain in this workspace or ~/.contenox (tried %s); run `contenox init` to seed %s, or point %s at a declared agent",
		c.chain, strings.Join(candidates, ", "), chainOracleDefaultFilename, configKeyOracleChain)
}

func oracleMountedLine(c oracleConfig) string {
	scope := "a subagent's questions"
	if c.approves {
		scope = "a subagent's questions AND its approve-tier tool calls"
	}
	return fmt.Sprintf("Oracle mounted (%s under %s): it may rule on %s as agent %q, within each subagent envelope's attention bounds.",
		c.chain, c.policy, scope, oracleAgentName)
}
