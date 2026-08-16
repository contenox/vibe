package agentdecl_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/stretchr/testify/require"
)

// writeDecl plants one declaration at dir/name.
func writeDecl(t *testing.T, dir, name, frontmatter, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	doc := "---\n" + frontmatter + "\n---\n\n" + body + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644))
}

// acpTree builds the shipped ACP chain's shape as a directory: a classifier
// over coding, review and general — which is what that chain's 12 tasks are.
func acpTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "acp")
	writeDecl(t, root, agentdecl.AgentFilename,
		"name: acp\ndescription: Route the prompt to the right loop\ndefault: general",
		"Classify the user's latest message.")

	writeDecl(t, filepath.Join(root, "coding"), agentdecl.AgentFilename,
		"name: coding\ndescription: Change code and files\ntools: Read, Write, Edit, Bash",
		"You change code.")
	writeDecl(t, filepath.Join(root, "coding"), agentdecl.RecoveryFilename,
		"name: coding-recovery\ndescription: Second attempt",
		"The first attempt failed. Try a smaller step.")

	// Deliberately NO recovery.md — the shipped review branch has none, and the
	// convention has to be able to express that absence.
	writeDecl(t, filepath.Join(root, "review"), agentdecl.AgentFilename,
		"name: review\ndescription: Read-only review\ntools: Read, Grep",
		"You review code without changing it.")

	writeDecl(t, filepath.Join(root, "general"), agentdecl.AgentFilename,
		"name: general\ndescription: Questions and explanations\ntools: Read",
		"You answer questions.")
	writeDecl(t, filepath.Join(root, "general"), agentdecl.RecoveryFilename,
		"name: general-recovery\ndescription: Second attempt",
		"The first attempt failed. Answer directly.")
	return root
}

func taskByID(chain *taskengine.TaskChainDefinition, id string) *taskengine.TaskDefinition {
	for i := range chain.Tasks {
		if chain.Tasks[i].ID == id {
			return &chain.Tasks[i]
		}
	}
	return nil
}

func gotoFor(task *taskengine.TaskDefinition, op taskengine.OperatorTerm, when string) string {
	for _, b := range task.Transition.Branches {
		if b.Operator == op && b.When == when {
			return b.Goto
		}
	}
	return ""
}

// The tree reproduces the shipped chain's shape: one router, one loop per
// branch, one shared terminal. This is the claim the whole convention rests on.
func TestUnit_Tree_ReproducesTheShippedRoutingShape(t *testing.T) {
	cfg := mustShipped(t)
	tree, err := agentdecl.LoadTree(acpTree(t), cfg)
	require.NoError(t, err)
	require.True(t, tree.IsRouter())
	require.Equal(t, []string{"coding", "general", "review"}, tree.Labels(),
		"children are sorted, so emission is deterministic")

	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	// The engine enters at the first task, so the router has to lead.
	require.Equal(t, "acp-route", chain.Tasks[0].ID)
	require.Equal(t, taskengine.HandleRoute, chain.Tasks[0].Handler)

	// One branch per directory, matched by the directory's own name.
	route := taskByID(chain, "acp-route")
	require.Equal(t, "acp-coding-agent", gotoFor(route, taskengine.OpEquals, "coding"))
	require.Equal(t, "acp-review-agent", gotoFor(route, taskengine.OpEquals, "review"))
	require.Equal(t, "acp-general-agent", gotoFor(route, taskengine.OpEquals, "general"))
	require.Equal(t, "acp-general-agent", gotoFor(route, taskengine.OpDefault, ""),
		"the declared default is where an unmapped answer lands")

	// Each branch is the same five-task loop a single declaration emits.
	coding := taskByID(chain, "acp-coding-agent")
	require.NotNil(t, coding)
	require.Equal(t, "acp-coding-tools", gotoFor(coding, taskengine.OpEquals, "tool_call"))
	require.NotNil(t, taskByID(chain, "acp-coding-recovery"))
	require.NotNil(t, taskByID(chain, "acp-coding-recovery-tools"))

	// ONE terminal for the whole tree, however many branches feed it.
	require.NotNil(t, taskByID(chain, "acp-summarise"))
	terminals := 0
	for _, task := range chain.Tasks {
		if task.ID == "acp-summarise" {
			terminals++
		}
	}
	require.Equal(t, 1, terminals)
}

// The shipped review branch has no recovery: an exhausted turn goes straight to
// the terminal. Absence of recovery.md has to mean exactly that.
func TestUnit_Tree_ALeafWithoutRecoveryFallsStraightToTheTerminal(t *testing.T) {
	cfg := mustShipped(t)
	tree, err := agentdecl.LoadTree(acpTree(t), cfg)
	require.NoError(t, err)
	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	require.Nil(t, taskByID(chain, "acp-review-recovery"),
		"no recovery.md means no recovery stage is invented")
	review := taskByID(chain, "acp-review-agent")
	require.NotNil(t, review)
	require.Equal(t, "acp-summarise", review.Transition.OnFailure)
	require.Equal(t, "acp-summarise",
		gotoFor(review, taskengine.OpEdgeTraversedAtLeast, "60"),
		"an exhausted review loop lands on the terminal, not on a stage that does not exist")

	// While a branch that DOES declare one keeps its own second attempt.
	coding := taskByID(chain, "acp-coding-agent")
	require.Equal(t, "acp-coding-recovery", coding.Transition.OnFailure)
}

// The point of the tree: the classifier's labels come from the directory names,
// so the prompt and the branches cannot disagree. In the hand-written chains
// they are two independent strings.
func TestUnit_Tree_LabelsAreInjectedIntoTheClassifierPrompt(t *testing.T) {
	cfg := mustShipped(t)
	tree, err := agentdecl.LoadTree(acpTree(t), cfg)
	require.NoError(t, err)
	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	prompt := taskByID(chain, "acp-route").SystemInstruction
	require.Contains(t, prompt, "Classify the user's latest message.", "the author's own prompt survives")
	for _, label := range tree.Labels() {
		require.Contains(t, prompt, "- "+label, "every branch label reaches the prompt")
	}
	require.Contains(t, prompt, "Read-only review", "a branch's description explains when to pick it")
	require.Contains(t, prompt, "answer general", "the fallback is stated to the classifier too")

	// A router decides; it does not act.
	require.Empty(t, taskByID(chain, "acp-route").ExecuteConfig.Tools)
}

// A classifier answering something unmapped is ordinary. Guessing which branch
// gets it is how a request ends up in the wrong loop silently.
func TestUnit_Tree_RefusesARouterWithNoUsableDefault(t *testing.T) {
	cfg := mustShipped(t)
	root := filepath.Join(t.TempDir(), "acp")
	writeDecl(t, root, agentdecl.AgentFilename, "name: acp\ndescription: Router", "Classify.")
	writeDecl(t, filepath.Join(root, "a"), agentdecl.AgentFilename, "name: a\ndescription: A", "A.")
	writeDecl(t, filepath.Join(root, "b"), agentdecl.AgentFilename, "name: b\ndescription: B", "B.")

	_, err := agentdecl.LoadTree(root, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no default")

	writeDecl(t, root, agentdecl.AgentFilename, "name: acp\ndescription: Router\ndefault: nope", "Classify.")
	_, err = agentdecl.LoadTree(root, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one of")
}

// One child needs no ceremony: the only branch is the fallback.
func TestUnit_Tree_ASingleChildNeedsNoDeclaredDefault(t *testing.T) {
	cfg := mustShipped(t)
	root := filepath.Join(t.TempDir(), "solo")
	writeDecl(t, root, agentdecl.AgentFilename, "name: solo\ndescription: Router", "Classify.")
	writeDecl(t, filepath.Join(root, "only"), agentdecl.AgentFilename, "name: only\ndescription: Only", "Do it.")

	tree, err := agentdecl.LoadTree(root, cfg)
	require.NoError(t, err)
	require.Equal(t, "only", tree.Default)
}

// Nesting composes without any further concept — the thing the hand-written
// chains cannot do without writing more tasks by hand.
func TestUnit_Tree_NestsWithoutAFurtherConcept(t *testing.T) {
	cfg := mustShipped(t)
	root := filepath.Join(t.TempDir(), "top")
	writeDecl(t, root, agentdecl.AgentFilename, "name: top\ndescription: Top\ndefault: work", "Classify.")
	writeDecl(t, filepath.Join(root, "work"), agentdecl.AgentFilename,
		"name: work\ndescription: Work\ndefault: fast", "Classify again.")
	writeDecl(t, filepath.Join(root, "work", "fast"), agentdecl.AgentFilename, "name: fast\ndescription: Fast", "Go.")
	writeDecl(t, filepath.Join(root, "work", "slow"), agentdecl.AgentFilename, "name: slow\ndescription: Slow", "Go.")

	tree, err := agentdecl.LoadTree(root, cfg)
	require.NoError(t, err)
	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	require.NotNil(t, taskByID(chain, "top-route"))
	inner := taskByID(chain, "top-work-route")
	require.NotNil(t, inner, "a child that branches is a router too")
	require.Equal(t, "top-work-route", gotoFor(taskByID(chain, "top-route"), taskengine.OpEquals, "work"))
	require.Equal(t, "top-work-fast-agent", gotoFor(inner, taskengine.OpEquals, "fast"))
}

// A router has no loop of its own, so it has nothing to recover from; a
// recovery.md there is a mistake worth naming rather than ignoring.
func TestUnit_Tree_RefusesRecoveryOnARouter(t *testing.T) {
	cfg := mustShipped(t)
	root := filepath.Join(t.TempDir(), "acp")
	writeDecl(t, root, agentdecl.AgentFilename, "name: acp\ndescription: Router\ndefault: a", "Classify.")
	writeDecl(t, root, agentdecl.RecoveryFilename, "name: oops\ndescription: Nope", "No.")
	writeDecl(t, filepath.Join(root, "a"), agentdecl.AgentFilename, "name: a\ndescription: A", "A.")

	_, err := agentdecl.LoadTree(root, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "router has no recovery")
}

// mustShipped is the embedded agents.toml — the same defaults a real run uses,
// so these tests cannot pass against a configuration nobody ships.
func mustShipped(t *testing.T) agentdecl.Config {
	t.Helper()
	cfg, err := agentdecl.Shipped()
	require.NoError(t, err)
	return cfg
}

// Sync must treat a tree as ONE agent. Without the skip it walks into the
// subtree and emits a chain per .md — six chains for what is one agent, three
// of them recovery prompts running as agents in their own right.
func TestUnit_Tree_SyncEmitsOneChainForTheWholeTree(t *testing.T) {
	cfg := mustShipped(t)
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents")

	// A tree...
	writeDecl(t, filepath.Join(agentsDir, "acp"), agentdecl.AgentFilename,
		"name: acp\ndescription: Router\ndefault: general", "Classify.")
	writeDecl(t, filepath.Join(agentsDir, "acp", "coding"), agentdecl.AgentFilename,
		"name: coding\ndescription: Coding", "Code.")
	writeDecl(t, filepath.Join(agentsDir, "acp", "coding"), agentdecl.RecoveryFilename,
		"name: coding-recovery\ndescription: Retry", "Retry.")
	writeDecl(t, filepath.Join(agentsDir, "acp", "general"), agentdecl.AgentFilename,
		"name: general\ndescription: General", "Answer.")
	// ...beside an ordinary flat declaration, which must keep working.
	writeDecl(t, agentsDir, "solo.md", "name: solo\ndescription: Solo", "Solo.")

	generated := filepath.Join(root, ".generated")
	results, err := agentdecl.Sync(
		[]agentdecl.SourceDir{{Path: agentsDir, Native: true}}, generated, cfg)
	require.NoError(t, err)

	names := map[string]bool{}
	for _, r := range results {
		if r.Action == agentdecl.ActionRefused {
			t.Fatalf("refused %s: %s", r.Source, r.Reason)
		}
		names[r.Name] = true
	}
	require.True(t, names["acp"], "the tree emits one agent named for its directory")
	require.True(t, names["solo"], "a flat declaration beside a tree still works")
	require.False(t, names["coding"], "a branch is not an agent of its own")
	require.False(t, names["coding-recovery"], "a recovery prompt is not an agent of its own")
	require.Len(t, results, 2, "one tree + one flat file = two agents")

	// And exactly one chain file, carrying the router.
	chains, err := filepath.Glob(filepath.Join(generated, "chain-agent-acp*.json"))
	require.NoError(t, err)
	require.Len(t, chains, 1)
	raw, err := os.ReadFile(chains[0])
	require.NoError(t, err)
	require.Contains(t, string(raw), "acp-route")
	require.Contains(t, string(raw), "acp-coding-agent")
}

// contenox init has to establish the convention it recommends. Seeding only
// flat examples while documenting directories is the gap this closes: the
// shipped example now IS a tree, and it transpiles.
func TestUnit_Tree_PreseedShipsAWorkingTreeExample(t *testing.T) {
	dir := t.TempDir()
	created, err := agentdecl.Preseed(dir)
	require.NoError(t, err)

	root := filepath.Join(dir, agentdecl.NativeSourceDir, "triage")
	require.FileExists(t, filepath.Join(root, agentdecl.AgentFilename))
	require.FileExists(t, filepath.Join(root, "code", agentdecl.AgentFilename))
	require.FileExists(t, filepath.Join(root, "code", agentdecl.RecoveryFilename))
	require.FileExists(t, filepath.Join(root, "docs", agentdecl.AgentFilename))
	require.Contains(t, created, filepath.Join(root, "code", agentdecl.RecoveryFilename))

	// Seeded is not enough — it has to actually transpile.
	cfg := mustShipped(t)
	tree, err := agentdecl.LoadTree(root, cfg)
	require.NoError(t, err)
	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)
	require.Equal(t, "triage-route", chain.Tasks[0].ID)
	require.NotNil(t, taskByID(chain, "triage-code-recovery"))
	require.Nil(t, taskByID(chain, "triage-docs-recovery"), "docs declares no recovery")

	// A second pass leaves an edited file alone.
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", agentdecl.AgentFilename),
		[]byte("---\nname: docs\ndescription: Mine now\n---\n\nMine.\n"), 0o644))
	_, err = agentdecl.Preseed(dir)
	require.NoError(t, err)
	again, err := os.ReadFile(filepath.Join(root, "docs", agentdecl.AgentFilename))
	require.NoError(t, err)
	require.Contains(t, string(again), "Mine now")
}

// A chain that does not lint is not registered, and the failure is silent: the
// agent simply never appears. Every emitted tree has to pass the same linter
// the engine applies to a hand-written one.
func TestUnit_Tree_EmittedChainsLint(t *testing.T) {
	cfg := mustShipped(t)
	for _, name := range []string{"acp", "triage"} {
		t.Run(name, func(t *testing.T) {
			tree, err := agentdecl.LoadTree(filepath.Join("preseed", "agents", name), cfg)
			require.NoError(t, err)
			chain, err := agentdecl.EmitTree(tree, cfg)
			require.NoError(t, err)
			require.NoError(t, taskengine.LintChain(chain), "emitted chain must lint")
		})
	}
}

// The loop macros exist so a declaration never names an emitted task id and
// never states a budget of its own. Both were real defects: the shipped chains
// claimed "12 main rounds" while enforcing 60, and the first conversion of them
// hardcoded task ids that a directory rename would have broken.
func TestUnit_Tree_LoopMacrosResolveToThisLeafAndTheRealBudget(t *testing.T) {
	cfg := mustShipped(t)
	root := filepath.Join(t.TempDir(), "svc")
	writeDecl(t, root, agentdecl.AgentFilename, "name: svc\ndescription: Router\ndefault: work", "Classify.")
	writeDecl(t, filepath.Join(root, "work"), agentdecl.AgentFilename,
		"name: work\ndescription: Work", "Do it.")
	writeDecl(t, filepath.Join(root, "work"), agentdecl.RecoveryFilename,
		"name: work-recovery\ndescription: Retry",
		"Used {{rounds_used}} of {{main_rounds}} main and {{recovery_rounds_used}} of {{recovery_rounds}} recovery.")

	tree, err := agentdecl.LoadTree(root, cfg)
	require.NoError(t, err)
	chain, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	got := taskByID(chain, "svc-work-recovery").SystemInstruction
	// Counters become engine macros over THIS leaf's own edges.
	require.Contains(t, got, "{{edge_count:svc-work-agent->svc-work-tools}}")
	require.Contains(t, got, "{{edge_count:svc-work-recovery->svc-work-recovery-tools}}")
	// Budgets become the configured numbers, so the prompt cannot claim one the
	// chain does not enforce.
	require.Contains(t, got, fmt.Sprintf("of %d main", cfg.Chain.MainRounds))
	require.Contains(t, got, fmt.Sprintf("of %d recovery", cfg.Chain.RecoveryRounds))
	require.NotContains(t, got, "{{main_rounds}}")
	require.NoError(t, taskengine.LintChain(chain))
}

// The declarations shipped with contenox must use the macros, not literals —
// otherwise renaming a tree silently breaks its prompts, which is what the
// first conversion did.
func TestUnit_Tree_ShippedDeclarationsNameNoEmittedTaskIDs(t *testing.T) {
	err := filepath.WalkDir("preseed/agents", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		body, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		require.NotContainsf(t, string(body), "{{edge_count:", "%s names emitted task ids; use %s", p, agentdecl.MacroRoundsUsed)
		return nil
	})
	require.NoError(t, err)
}
