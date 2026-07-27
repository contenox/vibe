package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/ollamatokenizer"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/searchtool"
	"github.com/contenox/beam/internal/services/workspaceindex"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// workspace_search on the ENGINE PATH.
//
// The searchtool package proves the payload and workspaceindex proves the
// retrieval. Neither can see the thing that decides whether the tool is usable
// at all: a workspace_search call travelling through BuildEngine → the aggregate
// tools repo → the REAL HITL wrapper → the SHIPPED policy file on disk.
//
// This mirrors gointel_e2e_test.go deliberately — same harness shape, same
// blunt rule — because the registration decision is the same one: workspace
// search is registered always-on as a READ, and a read toolset that stops to ask
// permission is a read toolset nobody leaves enabled.
//
// What makes these assertions mean something is the CONTROL: the same call, same
// engine, same tool, under an envelope with no workspace rule must fall to
// default_action, and under an explicit deny rule must be denied. One envelope
// allowing and another denying is the only evidence that the FILE decided rather
// than a built-in fallback.
//
// Hermetic by construction: with no index in the test database, Query returns
// ErrNoIndex BEFORE it embeds anything (workspaceindex.Query resolves the active
// config first), so the allow path needs no model and no network — and the
// result it returns is the blueprint's runnable instruction, which is itself
// worth pinning. The live-model half is the ollama-gated test at the bottom.
// ---------------------------------------------------------------------------

// wsearchAsker is the approval callback an allow-tier toolset must never reach.
// It DENIES rather than prompting, so a regression fails loudly instead of
// hanging the suite on a read from a terminal that is not there.
type wsearchAsker struct {
	mu   sync.Mutex
	asks []hitlservice.ApprovalRequest
}

func (a *wsearchAsker) ask(_ context.Context, req hitlservice.ApprovalRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = append(a.asks, req)
	return false, nil
}

func (a *wsearchAsker) seen() []hitlservice.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hitlservice.ApprovalRequest(nil), a.asks...)
}

func (a *wsearchAsker) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asks = nil
}

// wsearchSink captures the engine's hitl_decision and approval_requested events.
// They are the only honest evidence of WHICH envelope evaluated a call and what
// it decided: a green tool result cannot tell an allow from a policy that was
// never consulted at all.
type wsearchSink struct {
	mu     sync.Mutex
	events []taskengine.TaskEvent
}

func (s *wsearchSink) PublishTaskEvent(_ context.Context, ev taskengine.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *wsearchSink) Wants(kind taskengine.TaskEventKind) bool {
	return kind == taskengine.TaskEventHITLDecision || kind == taskengine.TaskEventApprovalRequested
}

func (s *wsearchSink) drain() []taskengine.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.events
	s.events = nil
	return out
}

type wsearchHarness struct {
	engine      *Engine
	asker       *wsearchAsker
	sink        *wsearchSink
	db          libdb.DBManager
	contenoxDir string
	workspaceID string
}

// newWSearchHarness builds a REAL engine through BuildEngine with HITL on and
// the SHIPPED policy presets seeded into an isolated .contenox dir, which
// hitlPolicySource consults BEFORE the user's ~/.contenox — so these tests
// evaluate the envelopes this binary ships, not whatever is on the machine.
//
// prepare runs against the open database BEFORE the engine is built, which is
// the only window in which the `contenox config` keys the engine reads at
// construction (default-embed-model and friends) can be seeded.
func newWSearchHarness(t *testing.T, prepare func(*testing.T, libdb.DBManager), opts ...func(*chatOpts)) *wsearchHarness {
	t.Helper()

	// Belt: no interactive answer is obtainable from this process, so anything
	// that reaches for one by a route other than the injected asker fails at once
	// rather than parking the test.
	devnull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	realStdin := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() {
		os.Stdin = realStdin
		_ = devnull.Close()
	})

	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true))

	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wsearch-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	if prepare != nil {
		prepare(t, db)
	}

	asker := &wsearchAsker{}
	sink := &wsearchSink{}

	co := chatOpts{
		EffectiveDefaultModel:     "wsearch-e2e-fake-model",
		EffectiveDefaultProvider:  "ollama",
		EffectiveContext:          4096,
		EffectiveHITL:             true,
		EffectiveSkipBackendCycle: true,
		EffectiveAskApproval:      asker.ask,
		EffectiveTaskEventSink:    sink,
		ContenoxDir:               contenoxDir,
	}
	for _, o := range opts {
		o(&co)
	}

	engine, err := BuildEngine(ctx, db, co)
	require.NoError(t, err)

	h := &wsearchHarness{
		engine:      engine,
		asker:       asker,
		sink:        sink,
		db:          db,
		contenoxDir: contenoxDir,
		workspaceID: ResolveWorkspaceID(contenoxDir),
	}
	t.Cleanup(h.stop)
	return h
}

func (h *wsearchHarness) stop() {
	if h.engine != nil && h.engine.Stop != nil {
		h.engine.Stop()
		h.engine.Stop = nil
	}
}

// call runs one workspace_search through the engine's chain executor — the real
// dispatch, the real HITL wrapper and the shipped policy included.
func (h *wsearchHarness) call(ctx context.Context, args map[string]string) (any, error) {
	chain := &taskengine.TaskChainDefinition{
		ID:          "workspace-search-e2e",
		Description: "one workspace_search call through the real engine",
		Tasks: []taskengine.TaskDefinition{{
			ID:      "call",
			Handler: taskengine.HandleTools,
			Tools: &taskengine.ToolsCall{
				Name:     searchtool.ToolsProviderName,
				ToolName: searchtool.ToolSearch,
				Args:     args,
			},
			Transition: taskengine.TaskTransition{
				Branches: []taskengine.TransitionBranch{{Operator: "default", Goto: taskengine.TermEnd}},
			},
		}},
	}
	out, _, _, err := h.engine.TaskService.Execute(ctx, chain, map[string]any{}, taskengine.DataTypeJSON)
	return out, err
}

// decisionFor returns the single hitl_decision event recorded for the workspace
// toolset, failing when there is not exactly one: zero means the gate was
// skipped entirely, two means it ran twice.
func (h *wsearchHarness) decisionFor(t *testing.T, events []taskengine.TaskEvent) taskengine.TaskEvent {
	t.Helper()
	var decisions []taskengine.TaskEvent
	for _, ev := range events {
		if ev.Kind == taskengine.TaskEventHITLDecision && ev.HookName == searchtool.ToolsProviderName {
			decisions = append(decisions, ev)
		}
	}
	require.Lenf(t, decisions, 1, "expected exactly one HITL decision for %s, got %d — the gate was skipped or ran twice",
		searchtool.ToolSearch, len(decisions))
	return decisions[0]
}

// writePolicy drops a hand-written envelope into the harness's .contenox dir so
// a test can pin it by name, the same way an operator adds one.
func writeWSearchPolicy(t *testing.T, contenoxDir, name, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, name), []byte(body), 0o644))
}

// ---------------------------------------------------------------------------
// (a) the shipped envelopes: allow, no approval, the asker never invoked
// ---------------------------------------------------------------------------

// TestSystem_WorkspaceSearch_EnginePathAllowedWithoutAskingUnderShippedPolicies
// is the acceptance test for registering workspace_search always-on. The call
// runs through a real engine under each interactive envelope this binary ships,
// and every one must ALLOW it without raising an approval.
//
// It also pins the degradation the blueprint requires: with no index, the tool
// answers with a RESULT naming `contenox index`, not an error. That the payload
// comes back at all is the proof the call reached the tool rather than being
// answered by the policy.
func TestSystem_WorkspaceSearch_EnginePathAllowedWithoutAskingUnderShippedPolicies(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)

	for _, policy := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			// The per-request pin is the production mechanism an ACP session uses
			// to choose its envelope (acpsvc/prompt.go). Pinning it here means the
			// decision event below names the file that was actually consulted.
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
			require.NoError(t, err)

			events := h.sink.drain()
			for _, ev := range events {
				require.NotEqualf(t, taskengine.TaskEventApprovalRequested, ev.Kind,
					"%s: %s.%s RAISED AN APPROVAL — a read toolset that interrupts is a read toolset nobody enables",
					policy, ev.HookName, ev.ToolName)
			}

			d := h.decisionFor(t, events)
			require.Equal(t, searchtool.ToolSearch, d.ToolName, "%s: the decision was recorded for the wrong tool", policy)
			require.Equalf(t, string(hitlservice.ActionAllow), d.HITLAction,
				"%s: workspace.%s was %q, not allow — it is a read of files the agent may already read", policy, searchtool.ToolSearch, d.HITLAction)
			require.Equalf(t, policy, d.HITLPolicyName, "%s: a different envelope evaluated the call", policy)
			if d.HITLApprovalRequested != nil {
				require.Falsef(t, *d.HITLApprovalRequested, "%s: workspace_search requested an approval", policy)
			}
			require.Emptyf(t, h.asker.seen(), "%s: the approval callback was invoked", policy)

			// The call reached the TOOL: what came back is the tool's own payload,
			// carrying the runnable instruction rather than a policy verdict.
			res, ok := out.(*searchtool.Result)
			require.Truef(t, ok, "%s: result is %T, not the tool's payload — the call may have been answered by the policy", policy, out)
			require.Empty(t, res.Hits)
			require.Containsf(t, res.Note, "contenox index",
				"%s: a workspace with no index must answer with the command that fixes it: %q", policy, res.Note)
		})
	}
}

// ---------------------------------------------------------------------------
// (b) no workspace rule: the call falls to default_action
// ---------------------------------------------------------------------------

// TestSystem_WorkspaceSearch_NoRuleFallsToDefaultActionAndAsks removes the
// workspace rule and leaves default_action at "approve" — the state an operator
// lands in with a hand-written envelope, or with a preset from before this
// toolset existed. The call must then RAISE AN APPROVAL: nothing silently
// allows a toolset the envelope does not mention.
//
// This is the control that gives the test above its meaning. Same engine, same
// tool, same arguments; only the file on disk differs, and the outcome flips.
func TestSystem_WorkspaceSearch_NoRuleFallsToDefaultActionAndAsks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)
	writeWSearchPolicy(t, h.contenoxDir, "hitl-policy-norule.json", `{
	  "default_action": "approve",
	  "rules": [
	    {"tools": "local_fs", "tool": "read_file", "action": "allow"}
	  ]
	}`)

	ctx := hitlservice.WithPolicyName(context.Background(), "hitl-policy-norule.json")
	h.sink.drain()
	h.asker.reset()

	out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
	require.NoError(t, err, "a call held at an approval and then denied returns the denial, not a chain error")

	events := h.sink.drain()
	d := h.decisionFor(t, events)
	require.Equal(t, "hitl-policy-norule.json", d.HITLPolicyName)
	require.Equalf(t, string(hitlservice.ActionApprove), d.HITLAction,
		"an envelope with no workspace rule must fall to default_action=approve, got %q — a toolset the policy never mentions must not be silently allowed", d.HITLAction)

	asks := h.asker.seen()
	require.Lenf(t, asks, 1, "default_action=approve did not reach the asker: %d ask(s)", len(asks))
	require.Equal(t, searchtool.ToolsProviderName, asks[0].ToolsName)
	require.Equal(t, searchtool.ToolSearch, asks[0].ToolName)

	// The decision event RECORDS that an approval was raised. (The separate
	// approval_requested event belongs to the DURABLE path —
	// hitlservice.RequestApproval, which ACP and missions suspend on; the CLI's
	// inline asker does not publish one, so this flag plus the asker call above
	// is the whole evidence trail on this path.)
	require.NotNil(t, d.HITLApprovalRequested, "the decision does not record whether an approval was raised")
	require.True(t, *d.HITLApprovalRequested, "the decision says no approval was raised, but the call was held at one")

	// The asker denied, so the model gets the refusal — as a value it can read,
	// never as a chain error that ends the turn.
	require.IsTypef(t, "", out, "a refused call must return the denial message, got %T", out)
}

// ---------------------------------------------------------------------------
// (c) an explicit deny rule: denied, and the model gets the soft denial
// ---------------------------------------------------------------------------

// TestSystem_WorkspaceSearch_ExplicitDenyRuleIsHonoured pins the third verdict.
// An operator who does not want the agent reading the index writes one rule, and
// the toolset must go dark — without the asker being consulted (there is nothing
// to approve) and without the turn dying (the model is told, and carries on).
func TestSystem_WorkspaceSearch_ExplicitDenyRuleIsHonoured(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)
	writeWSearchPolicy(t, h.contenoxDir, "hitl-policy-denyws.json", `{
	  "default_action": "approve",
	  "rules": [
	    {"tools": "workspace", "action": "deny"}
	  ]
	}`)

	ctx := hitlservice.WithPolicyName(context.Background(), "hitl-policy-denyws.json")
	h.sink.drain()
	h.asker.reset()

	out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
	require.NoError(t, err, "a denied tool returns the deny MESSAGE, not a chain error that ends the turn")

	d := h.decisionFor(t, h.sink.drain())
	require.Equal(t, "hitl-policy-denyws.json", d.HITLPolicyName)
	require.Equalf(t, string(hitlservice.ActionDeny), d.HITLAction,
		"an explicit deny rule on the workspace toolset was not honoured: %q", d.HITLAction)

	require.Empty(t, h.asker.seen(), "a denied call must not consult the approver: there is nothing to approve")

	msg, ok := out.(string)
	require.Truef(t, ok, "a denied call must return the soft denial as a value the model can read, got %T", out)
	require.NotEmpty(t, msg)
}

// TestSystem_WorkspaceSearch_ShippedDenyFloorEnvelopesWithdrawTheToolset records
// what the OTHER shipped envelopes do, because it is a real operational fact and
// not this file's decision to change.
//
// hitl-policy-acpx.json (the untrusted-driver envelope) and
// hitl-policy-strict.json both have default_action "deny" and no workspace rule,
// so an agent under either gets NO workspace search at all — exactly as it gets
// no gointel (see gointel_e2e_test.go's envelope control). That is the
// envelope's call to make; what this pins is that the withdrawal is a clean
// policy DENY rather than a crash or a hang.
//
// FINDING, REPORTED NOT CHANGED: the two deny-floor presets are owned by the
// policy files, not by this feature.
func TestSystem_WorkspaceSearch_ShippedDenyFloorEnvelopesWithdrawTheToolset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping workspace_search engine e2e: builds a real engine")
	}

	h := newWSearchHarness(t, nil)

	for _, policy := range []string{"hitl-policy-acpx.json", "hitl-policy-strict.json"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			ctx := hitlservice.WithPolicyName(context.Background(), policy)
			h.sink.drain()
			h.asker.reset()

			out, err := h.call(ctx, map[string]string{"question": "where is retry backoff configured"})
			require.NoError(t, err)

			d := h.decisionFor(t, h.sink.drain())
			require.Equal(t, policy, d.HITLPolicyName)
			require.Equalf(t, string(hitlservice.ActionDeny), d.HITLAction,
				"%s has a deny floor and no workspace rule, so the call must be denied — if it is allowed, the pinned FILE is not what decided", policy)
			require.IsType(t, "", out, "%s: a denied call returns the deny message", policy)
			require.Empty(t, h.asker.seen(), "%s: a deny floor has no operator to prompt", policy)
		})
	}
}

// ---------------------------------------------------------------------------
// The preset UPGRADE path — an EXISTING install must receive the workspace rule
// ---------------------------------------------------------------------------

// TestSystem_WorkspaceSearch_PresetUpgradeDeliversTheWorkspaceRule is the half
// that decides whether anyone but a fresh install ever gets this working.
//
// Every machine that ran a previous build has hitl-policy-default.json ALREADY
// on disk, written before the workspace toolset existed. If the upgrade path
// does not refresh it, workspace_search falls to default_action on every
// existing install — an approval prompt per search, which reads as "the feature
// is broken" and is invisible to every unit test in the tree.
func TestSystem_WorkspaceSearch_PresetUpgradeDeliversTheWorkspaceRule(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")

	// An install from BEFORE this toolset shipped: the presets on disk are a
	// previous build's, and the state file records that this is what we wrote.
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))
	state := map[string]string{}
	for _, p := range HITLPolicyPresets {
		require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, p.Name), []byte(previous), 0o644))
		state[p.Name] = presetSHA(previous)
	}
	writePresetState(contenoxDir, state)

	// The upgrade an ordinary run performs — NOT the wizard's --force.
	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Emptyf(t, stale, "untouched presets were treated as hand-edited and held back: %v", stale)

	// The two INTERACTIVE envelopes must now carry the workspace rule.
	for _, name := range []string{"hitl-policy-default.json", "hitl-policy-acp.json"} {
		raw, err := os.ReadFile(filepath.Join(contenoxDir, name))
		require.NoError(t, err)
		require.Containsf(t, string(raw), `"workspace"`,
			"%s was not upgraded, so every existing install gets an approval prompt per workspace_search", name)

		var doc struct {
			Rules []struct {
				Tools  string `json:"tools"`
				Action string `json:"action"`
			} `json:"rules"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		var found bool
		for _, r := range doc.Rules {
			if r.Tools == searchtool.ToolsProviderName {
				found = true
				require.Equalf(t, "allow", r.Action, "%s: the workspace rule is %q, not allow", name, r.Action)
			}
		}
		require.Truef(t, found, "%s: no rule for the %q toolset", name, searchtool.ToolsProviderName)
	}
}

// TestSystem_WorkspaceSearch_PreStateFileInstallIsNamedNotSilentlyRewritten is
// the INVERSION of a finding that was pinned here as a test. The finding, in
// full, because the resolution only makes sense against it:
//
// REPRODUCED LIVE on the maintainer's own machine, 2026-07-27:
// `contenox beam .` raised an approval card for every workspace_search call.
// The chain was:
//
//  1. beam_cmd.go resolves its policy directory with globalContenoxDir() —
//     $HOME/.contenox, deliberately NOT the --data-dir (see its comment: "the
//     chain and policy loaders resolve via $HOME"). So a workspace-local
//     .contenox cannot be used to work around any of this.
//  2. It then calls writeEmbeddedHITLPolicies(dir, false) on every start.
//  3. That install's hitl-policy-default.json predates this toolset and has no
//     workspace rule, so the toolset falls to default_action: approve.
//  4. The upgrade CANNOT refresh it: .preset-state.json does not exist on
//     installs made before that file was introduced, so upgradeEmbeddedHITLPolicies
//     takes its `!ok` branch, cannot distinguish "untouched preset from an older
//     build" from "hand-edited by the operator", and correctly-but-unhelpfully
//     leaves the operator's file alone.
//
// THE RESOLUTION IS NOT "overwrite it anyway". The envelope is the operator's
// security boundary; a build that silently replaces a file it cannot prove is
// untouched has traded a nagging read tool for a much worse bug. So step (4)
// stands — and what changed is that the gap is now DETECTED SEMANTICALLY and
// SAID OUT LOUD: which shipped toolsets this envelope has no rule for at all,
// named by `contenox doctor` and by beam's startup line, with one narrow verb
// (`contenox init --refresh-policies`) that fixes it on request. See
// hitl_policy_staleness.go.
func TestSystem_WorkspaceSearch_PreStateFileInstallIsNamedNotSilentlyRewritten(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))

	// An install from before the workspace toolset AND before .preset-state.json:
	// the preset is a previous build's, and nothing recorded having written it.
	previous := `{"default_action":"approve","rules":[{"tools":"local_fs","tool":"read_file","action":"allow"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "hitl-policy-default.json"), []byte(previous), 0o644))
	require.NoFileExists(t, filepath.Join(contenoxDir, presetStateFile))

	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Containsf(t, stale, "hitl-policy-default.json",
		"the upgrade did not even NAME the preset it declined to refresh")

	raw, err := os.ReadFile(filepath.Join(contenoxDir, "hitl-policy-default.json"))
	require.NoError(t, err)
	require.Equal(t, previous, string(raw),
		"an unprovable envelope must survive the upgrade byte for byte")

	// THE INVERSION: the install is no longer left to discover this one approval
	// card at a time. The toolset is named, precisely, from the rules the file
	// does not have.
	detected := stalePolicyPresets([]string{contenoxDir})
	require.Len(t, detected, 1)
	require.Equal(t, "hitl-policy-default.json", detected[0].Name)
	require.Containsf(t, detected[0].Toolsets, searchtool.ToolsProviderName,
		"the workspace toolset is exactly what this envelope will nag about")

	notice := stalePolicyNotice("hitl-policy-default.json", []string{contenoxDir})
	require.Contains(t, notice, searchtool.ToolsProviderName)
	require.Contains(t, notice, "stops for approval")
	require.Contains(t, notice, RefreshPoliciesCommand)

	// Until the operator acts, the envelope still decides what it decides —
	// unchanged, and now explained rather than mysterious.
	svc := hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(contenoxDir), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err := svc.Evaluate(context.Background(), searchtool.ToolsProviderName, searchtool.ToolSearch,
		map[string]any{"question": "where is retry backoff configured"})
	require.NoError(t, err)
	require.Equal(t, hitlservice.ActionApprove, r.Action)

	// And the verb the notice names actually ends it — the search runs
	// unattended, and the notice never fires again.
	require.NoError(t, writeEmbeddedHITLPolicies(contenoxDir, true)) // contenox init --refresh-policies
	require.Empty(t, stalePolicyNotice("hitl-policy-default.json", []string{contenoxDir}))
	require.Empty(t, stalePolicyPresets([]string{contenoxDir}))

	svc = hitlservice.NewWithDefaultPolicy(hitlservice.NewFSPolicySource(contenoxDir), testTenant, nopKV{}, libtracker.NoopTracker{}, "hitl-policy-default.json")
	r, err = svc.Evaluate(context.Background(), searchtool.ToolsProviderName, searchtool.ToolSearch,
		map[string]any{"question": "where is retry backoff configured"})
	require.NoError(t, err)
	require.Equalf(t, hitlservice.ActionAllow, r.Action,
		"after the refresh a read toolset must stop asking")
}

// TestSystem_WorkspaceSearch_HandEditedPresetIsNeverOverwritten is the other
// half of the same rule, and the reason the upgrade cannot simply always write:
// an operator who tightened their own envelope must not have it silently
// replaced by a build that wants to add a rule.
func TestSystem_WorkspaceSearch_HandEditedPresetIsNeverOverwritten(t *testing.T) {
	contenoxDir := filepath.Join(t.TempDir(), ".contenox")
	require.NoError(t, os.MkdirAll(contenoxDir, 0o750))

	// Shipped once (so the state file records it), then hand-edited.
	_, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	edited := `{"default_action":"deny","rules":[]}`
	require.NoError(t, os.WriteFile(filepath.Join(contenoxDir, "hitl-policy-default.json"), []byte(edited), 0o644))

	stale, err := upgradeEmbeddedHITLPolicies(contenoxDir, false)
	require.NoError(t, err)
	require.Contains(t, stale, "hitl-policy-default.json", "a hand-edited preset must be REPORTED as stale, not silently kept")

	raw, err := os.ReadFile(filepath.Join(contenoxDir, "hitl-policy-default.json"))
	require.NoError(t, err)
	require.Equal(t, edited, string(raw), "the operator's envelope was overwritten by the upgrade")
}

// ---------------------------------------------------------------------------
// The live half: a REAL index, a REAL embedding model, through the engine path
// ---------------------------------------------------------------------------

// ollamaReachable reports whether a local ollama daemon is listening, so the
// live test SKIPS rather than fails on a machine without one.
func ollamaReachable() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:11434", 750*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestSystem_WorkspaceSearch_LiveIndexAnswersWithCitationsUnderTheShippedPolicy
// is the end-to-end shape a session actually runs: a real index built with a
// real embedding model, queried by the agent's tool through the real engine and
// the shipped envelope, coming back with citations the test verifies against the
// files it wrote.
//
// It uses the SAME composition the CLI uses — the engine's model route behind
// workspaceindex's Embedder seam — rather than a fake, because the failure this
// catches is precisely the one a fake cannot have: a dimension, a provider
// route, or a model name that only misbehaves against a live daemon.
func TestSystem_WorkspaceSearch_LiveIndexAnswersWithCitationsUnderTheShippedPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live workspace_search e2e: needs a real embedding model")
	}
	if !ollamaReachable() {
		t.Skip("skipping live workspace_search e2e: no ollama daemon on 127.0.0.1:11434")
	}

	// A workspace this test owns, with content whose right answer is known.
	root := t.TempDir()
	files := map[string]string{
		"docs/retry.md": "# Retry policy\n\nWhen a request fails we wait before trying again.\n" +
			"The delay doubles after each attempt, which is called exponential backoff.\n" +
			"The ceiling is thirty seconds so a stuck caller never waits forever.\n",
		"docs/colors.md": "# Palette\n\nThe brand colour is a deep blue used for headings and links.\n" +
			"Secondary colours are grey for borders and amber for warnings.\n",
		"cook/soup.md": "# Soup\n\nChop the onions, sweat them in butter, add stock and simmer.\n" +
			"Season at the end, never at the start, or the salt concentrates.\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	h := newWSearchHarness(t, func(t *testing.T, db libdb.DBManager) {
		ctx := context.Background()
		store := runtimetypes.New(db.WithoutTransaction())
		// Exactly what `contenox config set default-embed-model nomic-embed-text`
		// writes. These are the ONLY reachable way to select an embedding model:
		// enginesvc.Config.DefaultEmbedModel exists but BuildEngine never populates
		// it and no CLI flag sets it (reported), so the KV keys are the real path
		// and therefore the one this test exercises.
		require.NoError(t, clikv.SetString(ctx, store, "default-embed-model", "nomic-embed-text"))
		require.NoError(t, clikv.SetString(ctx, store, "default-embed-provider", "ollama"))
		// The backend the model route resolves through — the row `contenox backend
		// add ollama` writes. It must exist before the engine's backend cycle runs,
		// or no model is ever in runtime state to resolve against.
		require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
			ID:      "wsearch-e2e-ollama",
			Name:    "ollama",
			BaseURL: "http://localhost:11434",
			Type:    "ollama",
		}))
	}, func(o *chatOpts) {
		// The backend cycle must RUN: it is what populates runtime state, and
		// without it every embed call fails with "no models found in runtime state".
		o.EffectiveSkipBackendCycle = false
	})

	store := runtimetypes.New(h.db.WithoutTransaction())
	require.Equal(t, "nomic-embed-text", h.engine.EmbeddingModel.Name,
		"the engine did not resolve the configured embedding model")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Build the index over the SAME seam the CLI composes (index_cmd.go's
	// openWorkspaceIndex), writing into the same database the engine's bound
	// querier reads.
	svc := workspaceindex.New(
		store,
		workspaceindex.NewLLMRepoEmbedder(h.engine.Models, h.engine.EmbeddingModel.Name, h.engine.EmbeddingModel.Provider),
		ollamatokenizer.NewEstimateTokenizer(),
		workspaceindex.Config{EmbedModel: h.engine.EmbeddingModel.Name, EmbedProvider: h.engine.EmbeddingModel.Provider},
	)
	report, err := svc.Build(ctx, root, workspaceindex.BuildOptions{WorkspaceID: h.workspaceID})
	require.NoError(t, err, "the live index build failed — is nomic-embed-text pulled?")
	require.Greater(t, report.ChunksWritten, 0)
	t.Logf("live index: %d chunk(s), %d embed call(s), %v", report.ChunksWritten, report.EmbedCalls, report.Duration)

	policy := "hitl-policy-default.json"
	qctx := hitlservice.WithPolicyName(ctx, policy)
	h.sink.drain()
	h.asker.reset()

	started := time.Now()
	out, err := h.call(qctx, map[string]string{"question": "how long does it wait between retries"})
	require.NoError(t, err)
	t.Logf("live workspace_search round trip: %v", time.Since(started))

	d := h.decisionFor(t, h.sink.drain())
	require.Equal(t, string(hitlservice.ActionAllow), d.HITLAction)
	require.Equal(t, policy, d.HITLPolicyName)
	require.Empty(t, h.asker.seen())

	res, ok := out.(*searchtool.Result)
	require.Truef(t, ok, "result is %T, not the tool's payload", out)
	require.NotEmpty(t, res.Hits, "a live index returned no hits for a question its corpus answers")

	// The top hit is the retry document, and it is a CITATION: a path and a line
	// range a human can open, never a floating blob.
	top := res.Hits[0]
	require.Equalf(t, "docs/retry.md", top.Path,
		"the top hit is %s, not the document that answers the question (scores: %s)", top.Path, wsearchScores(res))
	require.Greater(t, top.EndLine, 0)
	require.Equal(t, fmt.Sprintf("%s:%d-%d", top.Path, top.StartLine, top.EndLine), top.Citation)
	require.Contains(t, strings.ToLower(top.Text), "backoff")
	require.False(t, top.Stale, "nothing was edited, so no hit may be stale")

	// STALENESS, live: edit the indexed file and the same hit must come back
	// FLAGGED rather than silently served as current.
	require.NoError(t, os.WriteFile(filepath.Join(root, "docs", "retry.md"),
		[]byte("# Retry policy\n\nRewritten after indexing: the backoff ceiling is now sixty seconds.\n"), 0o644))

	out, err = h.call(qctx, map[string]string{"question": "how long does it wait between retries"})
	require.NoError(t, err)
	res, ok = out.(*searchtool.Result)
	require.True(t, ok)
	require.NotEmpty(t, res.Hits)
	require.Greaterf(t, res.Stale, 0, "the file was rewritten after indexing and no hit was marked stale — a hit whose file changed underneath is a lie")
	require.Containsf(t, res.Note, "stale", "the payload does not tell the model its citations moved: %q", res.Note)
}

// wsearchScores renders the ranking for a failure message, so a quality
// regression reports WHAT it ranked instead of only that it was wrong.
func wsearchScores(res *searchtool.Result) string {
	var b strings.Builder
	for i, h := range res.Hits {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%.4f", h.Citation, h.Score)
	}
	return b.String()
}
