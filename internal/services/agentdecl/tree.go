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

// TreeDialect is how a tree's declarations are read. contenox's own
// frontmatter — name, description, tools, model, permissionMode — is the
// claude-code vocabulary, which is why files written for that tool run here
// unchanged; a tree node states the dialect instead of guessing it.
const TreeDialect = DialectClaudeCode

// AgentFilename is the declaration at a tree node. A directory holding one is a
// node; a directory holding subdirectories as well is a router over them.
const AgentFilename = "agent.md"

// RecoveryFilename is the OPTIONAL second attempt beside a leaf's agent.md.
//
// A file rather than a flag because a recovery prompt is a different prompt: it
// is written for an agent that has already failed once, and the shipped chains
// prove the need — their recovery stages carry their own instructions, and the
// review branch deliberately has none at all.
const RecoveryFilename = "recovery.md"

// FailureFilename is the OPTIONAL terminal prompt at a tree's ROOT: what the
// chain says when every branch has given up. Absent takes a plain default.
//
// One per tree, not per leaf, because there is one terminal: a reader wants to
// know what was attempted, and which branch attempted it is already in the
// transcript.
const FailureFilename = "failure.md"

// AgentTree is a directory of declarations: one agent, or a router over the
// agents beneath it.
//
// THE TREE IS THE BRANCHING. A chain that routes between a coding loop, a
// review loop and a general loop is a directory with three subdirectories, and
// the classifier that chooses between them is the agent.md at their parent.
// Nothing in the declaration format had to grow a "branches" field, and nesting
// composes without any further concept.
type AgentTree struct {
	// Name is the directory's name, and for a child it is ALSO the branch label
	// the router matches on. They cannot drift because they are one string.
	Name string
	// Agent is this node's declaration: a leaf's agent, or a router's classifier.
	Agent *AgentIR
	// Recovery is the leaf's second attempt, or nil. Always nil on a router.
	Recovery *AgentIR
	// Children are the branches, sorted by name so emission is deterministic.
	Children []*AgentTree
	// Default names the child a router falls back to when the classifier answers
	// something unmapped. Taken from the router declaration's `default:` field.
	Default string
	// Failure is the tree's terminal prompt, read from failure.md at the root.
	// Nil anywhere else.
	Failure *AgentIR
}

// IsRouter reports whether this node branches.
func (t *AgentTree) IsRouter() bool { return len(t.Children) > 0 }

// Labels are the branch labels this router accepts, in emission order.
func (t *AgentTree) Labels() []string {
	out := make([]string, 0, len(t.Children))
	for _, c := range t.Children {
		out = append(out, c.Name)
	}
	return out
}

// LoadTree reads a directory as an agent tree.
//
// A directory qualifies when it holds an agent.md. Subdirectories that hold one
// become branches; anything else in the directory is ignored, so a tree can
// carry a README or fixtures without becoming part of the chain.
func LoadTree(dir string, cfg Config) (*AgentTree, error) {
	agentPath := filepath.Join(dir, AgentFilename)
	data, err := os.ReadFile(agentPath)
	if err != nil {
		return nil, fmt.Errorf("agentdecl: %s: %w", agentPath, err)
	}
	// Parsed AS the native dialect rather than sniffed: a file at a tree node is
	// contenox's own declaration by construction, and DetectDialect refuses a
	// file that carries no vendor-specific field and sits outside a known
	// vendor directory — which every tree file does.
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

// validateDefault refuses a router whose fallback names no child.
//
// Refused rather than guessed: a classifier answering something unmapped is
// ordinary, and silently routing it to whichever branch sorted first is how a
// request ends up in the wrong loop with nothing saying so.
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

// EmitTree renders a tree as ONE task chain: a router per branching node, the
// five-task loop per leaf, and a single terminal they all fall back to.
//
// The emitted chain is the same shape as the hand-written ones it replaces —
// see the equivalence test, which diffs this against the shipped ACP chain.
func EmitTree(root *AgentTree, cfg Config) (*taskengine.TaskChainDefinition, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// A tree node's identity is its PATH, not its dialect. ScopedName exists to
	// keep an agent imported out of another tool's directory from colliding with
	// a native one; a tree is native by construction and its directory names are
	// already unique among their siblings, so prefixing here would only make the
	// task ids depend on which vocabulary the file happened to be written in.
	// The declaration's own name wins, and the directory is the fallback. A
	// shipped agent is looked up BY NAME — chain-acp — so the
	// emitted id has to be the name it is already known by, or converting an
	// agent silently renames it and every reference to it stops resolving.
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
	// The terminal reads whatever ran last, not a named task: several branches
	// reach it and they do not share a stage — the review branch has no recovery
	// at all, so naming one would point at a task that does not exist on the
	// path that got here. The shipped chains use the same "previous_output".
	_ = lastLoop
	term := terminalTask(terminal, cfg, "previous_output")
	if root.Failure != nil {
		term.SystemInstruction = systemInstruction(root.Failure)
	}
	tasks = append(tasks, term)

	// The engine enters at the first task, so the entry has to lead.
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

// emitNode appends a node's tasks and reports the id to enter it by, plus the
// id of the last loop emitted — which the terminal reads its input from.
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
			// A classifier that FAILS falls through to the default branch. The
			// alternative is a chain that dies because the cheap model used to
			// pick a lane was briefly unavailable — the work is still doable,
			// just not sorted, so it goes where unsorted work goes.
			OnFailure: defaultGoto,
			Branches:  branches,
		},
	}, taskengine.TaskDefinition{})
	// Drop the placeholder appended above; it exists only to keep the router
	// adjacent to its branches in the emitted order.
	*tasks = (*tasks)[:len(*tasks)-1]
	return routeID, lastLoop, nil
}

// routerInstruction is the classifier prompt with the labels APPENDED FROM THE
// DIRECTORY NAMES.
//
// This is the whole reason the tree is worth having. In the hand-written chains
// the prompt names its labels in prose while the branches match the same
// strings by equality, and nothing checks that the two agree — a renamed branch
// silently stops being reachable. Here one string is both, so they cannot
// disagree, and an author cannot forget to update the prompt.
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

// routerConfig runs classification on the alt model when one is configured:
// choosing a branch is a one-word answer and does not need the model the branch
// itself will use. It carries NO tools — a router decides, it does not act.
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

// MergedIR is the tree seen as one agent, for the things that are decided per
// CHAIN rather than per task: its name, and the HITL policy generated beside
// it.
//
// Tools are the UNION of every leaf's. A policy is written once for the chain
// and any branch may run, so a policy narrowed to the router — which holds no
// tools at all — would refuse the work the branches exist to do. The posture is
// the broadest any leaf declares, for the same reason.
//
// The hash covers every node, so editing any file in the tree regenerates.
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
	// A tool is denied for the chain only when EVERY leaf denies it: one branch
	// that may use it means the policy has to allow it through.
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
