package agentdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// TreeDialect is the dialect a tree's declarations are read as.
const TreeDialect = DialectClaudeCode

// AgentFilename is the declaration at a tree node. A directory holding one is a
// node; a directory holding subdirectories as well is a router over them.
const AgentFilename = "agent.md"

// RecoveryFilename is the optional second attempt beside a leaf's agent.md.
const RecoveryFilename = "recovery.md"

// FailureFilename is the optional terminal prompt at a tree's root: what the
// chain says when every branch has given up.
const FailureFilename = "failure.md"

// AgentTree is a directory of declarations: one agent, or a router over the
// agents beneath it.
type AgentTree struct {
	// Name is the directory's name, and for a child also the branch label the
	// router matches on.
	Name string
	// Agent is this node's declaration: a leaf's agent, or a router's classifier.
	Agent *AgentIR
	// Recovery is the leaf's second attempt, or nil. Always nil on a router.
	Recovery *AgentIR
	// Children are the branches, sorted by name so emission is deterministic.
	Children []*AgentTree
	// Default names the child a router falls back to when the classifier answers
	// something unmapped.
	Default string
	// Failure is the tree's terminal prompt, read from failure.md at the root.
	Failure *AgentIR
}

// IsRouter reports whether this node branches.
func (t *AgentTree) IsRouter() bool { return len(t.Children) > 0 }

// Labels returns the branch labels this router accepts, in emission order.
func (t *AgentTree) Labels() []string {
	out := make([]string, 0, len(t.Children))
	for _, c := range t.Children {
		out = append(out, c.Name)
	}
	return out
}

// LoadTree reads a directory as an agent tree. A directory qualifies when it
// holds an agent.md; subdirectories that hold one become branches.
func LoadTree(dir string, cfg Config) (*AgentTree, error) {
	agentPath := filepath.Join(dir, AgentFilename)
	data, err := os.ReadFile(agentPath)
	if err != nil {
		return nil, fmt.Errorf("agentdecl: %s: %w", agentPath, err)
	}
	// Parsed as the native dialect rather than sniffed: a file at a tree node is
	// contenox's own declaration by construction.
	ir, err := ParseAs(TreeDialect, agentPath, data, cfg)
	if err != nil {
		return nil, err
	}
	node := &AgentTree{Name: filepath.Base(dir), Agent: ir, Default: ir.DefaultBranch}

	if rec, rErr := os.ReadFile(filepath.Join(dir, RecoveryFilename)); rErr == nil {
		recIR, pErr := ParseAs(TreeDialect, filepath.Join(dir, RecoveryFilename), rec, cfg)
		if pErr != nil {
			return nil, pErr
		}
		node.Recovery = recIR
	}

	if fail, fErr := os.ReadFile(filepath.Join(dir, FailureFilename)); fErr == nil {
		failIR, pErr := ParseAs(TreeDialect, filepath.Join(dir, FailureFilename), fail, cfg)
		if pErr != nil {
			return nil, pErr
		}
		node.Failure = failIR
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("agentdecl: read %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		if _, sErr := os.Stat(filepath.Join(child, AgentFilename)); sErr != nil {
			continue
		}
		sub, sErr := LoadTree(child, cfg)
		if sErr != nil {
			return nil, sErr
		}
		node.Children = append(node.Children, sub)
	}

	if node.IsRouter() {
		if node.Recovery != nil {
			return nil, fmt.Errorf("agentdecl: %s: a router has no recovery of its own — put %s beside the leaf that needs it",
				dir, RecoveryFilename)
		}
		if err := node.validateDefault(dir); err != nil {
			return nil, err
		}
	}
	return node, nil
}

func (t *AgentTree) validateDefault(dir string) error {
	if t.Default == "" {
		if len(t.Children) == 1 {
			t.Default = t.Children[0].Name
			return nil
		}
		return fmt.Errorf("agentdecl: %s routes to %s and names no default — add `default: <one of them>` to its %s",
			dir, strings.Join(t.Labels(), ", "), AgentFilename)
	}
	for _, c := range t.Children {
		if c.Name == t.Default {
			return nil
		}
	}
	return fmt.Errorf("agentdecl: %s declares default %q, which is not one of %s",
		dir, t.Default, strings.Join(t.Labels(), ", "))
}

// EmitTree renders a tree as one task chain: a router per branching node, the
// five-task loop per leaf, and a single terminal they all fall back to.
func EmitTree(root *AgentTree, cfg Config) (*taskengine.TaskChainDefinition, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// The declaration's own name wins over the directory: a shipped agent is
	// looked up by name, so emitting anything else would rename it.
	id := root.Agent.Name
	if id == "" {
		id = root.Name
	}
	terminal := id + "-summarise"

	var tasks []taskengine.TaskDefinition
	entry, lastLoop, err := emitNode(root, cfg, id, terminal, &tasks)
	if err != nil {
		return nil, err
	}
	// The terminal reads whatever ran last, not a named task: branches reaching
	// it do not share a stage.
	_ = lastLoop
	term := terminalTask(terminal, cfg, "previous_output")
	if root.Failure != nil {
		term.SystemInstruction = systemInstruction(root.Failure)
	}
	tasks = append(tasks, term)

	// The engine enters at the first task.
	if tasks[0].ID != entry {
		for i, t := range tasks {
			if t.ID == entry {
				tasks[0], tasks[i] = tasks[i], tasks[0]
				break
			}
		}
	}
	return &taskengine.TaskChainDefinition{
		ID:          id,
		Description: root.Agent.Description,
		TokenLimit:  cfg.Chain.TokenLimit,
		Tasks:       tasks,
	}, nil
}

func emitNode(node *AgentTree, cfg Config, prefix, terminal string, tasks *[]taskengine.TaskDefinition) (string, string, error) {
	if err := refuseUnsafe(node.Agent); err != nil {
		return "", "", err
	}
	if !node.IsRouter() {
		*tasks = append(*tasks, leafLoop(node.Agent, node.Recovery, cfg, prefix, terminal)...)
		return prefix + "-agent", prefix + "-recovery", nil
	}

	routeID := prefix + "-route"
	branches := make([]taskengine.TransitionBranch, 0, len(node.Children)+1)
	var defaultGoto, lastLoop string
	for _, child := range node.Children {
		childEntry, childLast, err := emitNode(child, cfg, prefix+"-"+child.Name, terminal, tasks)
		if err != nil {
			return "", "", err
		}
		branches = append(branches, taskengine.TransitionBranch{
			Operator: taskengine.OpEquals, When: child.Name, Goto: childEntry,
		})
		if child.Name == node.Default {
			defaultGoto = childEntry
		}
		lastLoop = childLast
	}
	branches = append(branches, taskengine.TransitionBranch{
		Operator: taskengine.OpDefault, When: "", Goto: defaultGoto,
	})

	*tasks = append(*tasks, taskengine.TaskDefinition{
		ID:                routeID,
		Description:       node.Agent.Description,
		Handler:           taskengine.HandleRoute,
		SystemInstruction: routerInstruction(node),
		ExecuteConfig:     routerConfig(cfg),
		Transition: taskengine.TaskTransition{
			// A classifier that fails falls through to the default branch.
			OnFailure: defaultGoto,
			Branches:  branches,
		},
	}, taskengine.TaskDefinition{})
	// Drop the placeholder appended above; it exists only to keep the router
	// adjacent to its branches in the emitted order.
	*tasks = (*tasks)[:len(*tasks)-1]
	return routeID, lastLoop, nil
}

func routerInstruction(node *AgentTree) string {
	body := strings.TrimRight(systemInstruction(node.Agent), "\n")
	labels := node.Labels()
	lines := make([]string, 0, len(labels))
	for _, c := range node.Children {
		desc := strings.TrimSpace(c.Agent.Description)
		if desc == "" {
			lines = append(lines, "- "+c.Name)
			continue
		}
		lines = append(lines, "- "+c.Name+": "+desc)
	}
	return body + "\n\nAnswer with exactly one of these labels and nothing else:\n" +
		strings.Join(lines, "\n") + "\n\nIf none clearly applies, answer " + node.Default + "."
}

func routerConfig(cfg Config) *taskengine.LLMExecutionConfig {
	model, provider := cfg.Routing.RouterModel, cfg.Routing.RouterProvider
	if model == "" {
		model = cfg.Routing.Model
	}
	if provider == "" {
		provider = cfg.Routing.Provider
	}
	return &taskengine.LLMExecutionConfig{
		Model:            model,
		Provider:         provider,
		Tools:            []string{},
		PassClientsTools: false,
	}
}

// MergedIR is the tree seen as one agent, for what is decided per chain rather
// than per task. Tools are the union of every leaf's, the posture the broadest,
// and the hash covers every node.
func (t *AgentTree) MergedIR() *AgentIR {
	out := &AgentIR{
		Name:        t.Name,
		Description: t.Agent.Description,
		Source:      t.Agent.Source,
	}
	sum := sha256.New()
	var walk func(n *AgentTree)
	walk = func(n *AgentTree) {
		for _, ir := range []*AgentIR{n.Agent, n.Recovery, n.Failure} {
			if ir == nil {
				continue
			}
			sum.Write([]byte(ir.Source.SHA256))
		}
		if !n.IsRouter() {
			if n.Agent.Tools.Inherit {
				out.Tools.Inherit = true
			}
			out.Tools.Allow = append(out.Tools.Allow, n.Agent.Tools.Allow...)
			if n.Agent.Posture > out.Posture {
				out.Posture = n.Agent.Posture
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(t)
	out.Tools.Allow = dedupeSorted(out.Tools.Allow)
	// A tool is denied for the chain only when every leaf denies it.
	out.Tools.Deny = commonDeny(t)
	out.Source.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return out
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func commonDeny(root *AgentTree) []string {
	var leaves []*AgentTree
	var walk func(n *AgentTree)
	walk = func(n *AgentTree) {
		if !n.IsRouter() {
			leaves = append(leaves, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	if len(leaves) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, l := range leaves {
		for _, d := range dedupeSorted(l.Agent.Tools.Deny) {
			counts[d]++
		}
	}
	var out []string
	for tool, n := range counts {
		if n == len(leaves) {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}
