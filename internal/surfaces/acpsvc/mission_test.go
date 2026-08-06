package acpsvc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
)

// fakeDispatcher records the mission it was asked to fire and returns a canned
// result — the narrow MissionDispatcher slice makes it a two-field struct.
type fakeDispatcher struct {
	got    fleetservice.DispatchRequest
	result fleetservice.DispatchResult
	err    error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req fleetservice.DispatchRequest) (fleetservice.DispatchResult, error) {
	f.got = req
	if f.err != nil {
		return fleetservice.DispatchResult{}, f.err
	}
	return f.result, nil
}

// fakeResolver resolves the names in known to an Agent, everything else to a
// not-found error — the shape agentregistryservice.GetByName has.
type fakeResolver struct {
	known map[string]bool
}

func (f *fakeResolver) GetByName(_ context.Context, name string) (*runtimetypes.Agent, error) {
	if f.known[name] {
		return &runtimetypes.Agent{Name: name}, nil
	}
	return nil, libdb.ErrNotFound
}

// fakeEnvelopes stands in for the host's policy search path with two
// envelopes of opposite character, so a listing assertion can tell them apart.
type fakeEnvelopes struct{}

func (fakeEnvelopes) ListEnvelopes() []MissionEnvelope {
	return []MissionEnvelope{
		{Name: "hitl-policy-default.json", Path: "/cfg/hitl-policy-default.json", Summary: "unruled calls stop for approval · an agent may answer 3 questions"},
		{Name: "hitl-policy-strict.json", Path: "/cfg/hitl-policy-strict.json", Summary: "unruled calls are denied · questions always wait for a human"},
	}
}

func (f fakeEnvelopes) LookupEnvelope(name string) (MissionEnvelope, bool) {
	for _, e := range f.ListEnvelopes() {
		if e.Name == name {
			return e, true
		}
	}
	return MissionEnvelope{}, false
}

func newMissionTestTransport(t *testing.T, disp *fakeDispatcher, res *fakeResolver) (*Transport, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mission-acp.db")
	db, err := libdb.NewSQLiteDBManager(ctx, path, runtimetypes.SchemaSQLite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tr := &Transport{deps: Deps{DB: db}}
	if disp != nil {
		tr.deps.Fleet = disp
	}
	if res != nil {
		tr.deps.Agents = res
	}
	return tr, db
}

func setMissionConfig(t *testing.T, db libdb.DBManager, key, value string) {
	t.Helper()
	store := runtimetypes.New(db.WithoutTransaction())
	if err := clikv.WriteConfig(context.Background(), store, "", key, value); err != nil {
		t.Fatalf("seed config %s: %v", key, err)
	}
}

func TestUnit_HandleMission_DefaultAgentForm(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{InstanceID: "inst-1", SessionID: "sess-1", MissionID: "m-1"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "hitl-policy-strict.json")

	sess := &sessionEntry{InternalSessionID: "cnx-parent-1"}
	out, err := tr.handleMission(context.Background(), sess, "triage the failing CI run")
	if err != nil {
		t.Fatalf("handleMission: %v", err)
	}

	if disp.got.AgentName != "reviewer" {
		t.Fatalf("agent = %q, want reviewer", disp.got.AgentName)
	}
	if disp.got.Intent != "triage the failing CI run" {
		t.Fatalf("intent = %q", disp.got.Intent)
	}
	if disp.got.HITLPolicyName != "hitl-policy-strict.json" {
		t.Fatalf("policy = %q", disp.got.HITLPolicyName)
	}
	if disp.got.ParentSessionID != "cnx-parent-1" {
		t.Fatalf("parent session = %q, want cnx-parent-1", disp.got.ParentSessionID)
	}
	for _, want := range []string{"default mission agent", "reviewer", "triage the failing CI run", "m-1", "inst-1", "sess-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirmation %q missing %q", out, want)
		}
	}
}

func TestUnit_HandleMission_NamedAgentForm(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{MissionID: "m-2"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{known: map[string]bool{"planner": true}})
	setMissionConfig(t, db, "default-mission-agent", "reviewer") // present, but must be overridden
	setMissionConfig(t, db, "default-mission-policy", "envelope.json")

	sess := &sessionEntry{InternalSessionID: "cnx-parent-2"}
	out, err := tr.handleMission(context.Background(), sess, "planner draft the release notes")
	if err != nil {
		t.Fatalf("handleMission: %v", err)
	}

	if disp.got.AgentName != "planner" {
		t.Fatalf("agent = %q, want planner", disp.got.AgentName)
	}
	if disp.got.Intent != "draft the release notes" {
		t.Fatalf("intent = %q, want 'draft the release notes'", disp.got.Intent)
	}
	if !strings.Contains(out, "named agent") || !strings.Contains(out, "planner") {
		t.Fatalf("confirmation must name the chosen agent: %q", out)
	}
}

func TestUnit_HandleMission_UnknownFirstTokenFallsBackToDefault(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{MissionID: "m-3"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{known: map[string]bool{"planner": true}})
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "envelope.json")

	sess := &sessionEntry{InternalSessionID: "cnx-3"}
	if _, err := tr.handleMission(context.Background(), sess, "summarise today's commits"); err != nil {
		t.Fatalf("handleMission: %v", err)
	}
	if disp.got.AgentName != "reviewer" {
		t.Fatalf("agent = %q, want reviewer (default)", disp.got.AgentName)
	}
	if disp.got.Intent != "summarise today's commits" {
		t.Fatalf("intent = %q", disp.got.Intent)
	}
}

// TestUnit_HandleMission_InProcessConfirmationNamesThisSession pins: the confirmation names this session as where reports arrive live, not the inbox.
func TestUnit_HandleMission_InProcessConfirmationNamesThisSession(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{InstanceID: "i", SessionID: "s", MissionID: "m"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "envelope.json")

	out, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "cnx-1"}, "go")
	if err != nil {
		t.Fatalf("handleMission: %v", err)
	}
	if !strings.Contains(out, "live in this session") {
		t.Fatalf("in-process confirmation must say reports arrive live in this session: %q", out)
	}
}

func TestUnit_HandleMission_NoDefaultAgentErrors(t *testing.T) {
	tr, db := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	setMissionConfig(t, db, "default-mission-policy", "envelope.json")
	_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"}, "do something")
	if err == nil || !strings.Contains(err.Error(), "no mission agent") {
		t.Fatalf("want no-agent error, got %v", err)
	}
}

func TestUnit_HandleMission_NoEnvelopeErrors(t *testing.T) {
	tr, db := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"}, "do something")
	if err == nil || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("want no-envelope error, got %v", err)
	}
}

func TestUnit_HandleMission_UnavailableWithoutFleet(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil) // Fleet left nil (stdio acp path)
	_, err := tr.handleMission(context.Background(), &sessionEntry{}, "do something")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("want unavailable error, got %v", err)
	}
}

// TestUnit_HandleMission_EmptyArgsListsEnvelopes pins the discovery surface:
// `/mission` alone fires nothing and answers with the grammar, the defaults in
// force, and every envelope on the policy search path with its character.
func TestUnit_HandleMission_EmptyArgsListsEnvelopes(t *testing.T) {
	disp := &fakeDispatcher{}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	tr.deps.MissionEnvelopes = fakeEnvelopes{}
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "hitl-policy-strict.json")

	out, err := tr.handleMission(context.Background(), &sessionEntry{}, "   ")
	if err != nil {
		t.Fatalf("bare /mission must not error: %v", err)
	}
	if disp.got.AgentName != "" {
		t.Fatalf("bare /mission must fire nothing, dispatched %+v", disp.got)
	}
	for _, want := range []string{
		"--policy <envelope>",
		"reviewer",
		"hitl-policy-default.json",
		"unruled calls stop for approval",
		"hitl-policy-strict.json",
		"questions always wait for a human",
		"* hitl-policy-strict.json", // the configured default is marked
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
}

// TestUnit_HandleMission_EmptyArgsWithoutEnvelopeSourceStillTeaches pins that a
// host wiring no envelope source still answers a bare /mission with the grammar.
func TestUnit_HandleMission_EmptyArgsWithoutEnvelopeSourceStillTeaches(t *testing.T) {
	tr, _ := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	out, err := tr.handleMission(context.Background(), &sessionEntry{}, "")
	if err != nil {
		t.Fatalf("bare /mission must not error: %v", err)
	}
	if !strings.Contains(out, "/mission <intent>") {
		t.Fatalf("listing missing the grammar:\n%s", out)
	}
}

// TestUnit_HandleMission_PolicyFlagOverridesTheDefault pins that --policy
// bounds this one mission and the confirmation says where the envelope came
// from — the CLI prints agent+envelope on every fire, and a session must not be
// quieter about the bounds it just accepted.
func TestUnit_HandleMission_PolicyFlagOverridesTheDefault(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{MissionID: "m-9"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{known: map[string]bool{"planner": true}})
	tr.deps.MissionEnvelopes = fakeEnvelopes{}
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "hitl-policy-default.json")

	out, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "cnx-9"},
		"--policy hitl-policy-strict.json planner draft the release notes")
	if err != nil {
		t.Fatalf("handleMission: %v", err)
	}
	if disp.got.HITLPolicyName != "hitl-policy-strict.json" {
		t.Fatalf("policy = %q, want the flag's envelope", disp.got.HITLPolicyName)
	}
	if disp.got.AgentName != "planner" || disp.got.Intent != "draft the release notes" {
		t.Fatalf("flags must not be swallowed by the agent/intent split: agent=%q intent=%q", disp.got.AgentName, disp.got.Intent)
	}
	for _, want := range []string{"named agent", "planner", "hitl-policy-strict.json", "--policy", "questions always wait for a human"} {
		if !strings.Contains(out, want) {
			t.Fatalf("confirmation missing %q:\n%s", want, out)
		}
	}
}

// TestUnit_HandleMission_PolicyFlagEqualsForm pins the `--policy=<name>` spelling.
func TestUnit_HandleMission_PolicyFlagEqualsForm(t *testing.T) {
	disp := &fakeDispatcher{result: fleetservice.DispatchResult{MissionID: "m-10"}}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	tr.deps.MissionEnvelopes = fakeEnvelopes{}
	setMissionConfig(t, db, "default-mission-agent", "reviewer")

	if _, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "cnx-10"},
		"--policy=hitl-policy-strict.json triage the failing CI run"); err != nil {
		t.Fatalf("handleMission: %v", err)
	}
	if disp.got.HITLPolicyName != "hitl-policy-strict.json" {
		t.Fatalf("policy = %q", disp.got.HITLPolicyName)
	}
	if disp.got.Intent != "triage the failing CI run" {
		t.Fatalf("intent = %q", disp.got.Intent)
	}
}

// TestUnit_HandleMission_UnknownEnvelopeIsRefusedWithTheList pins: a typo'd
// envelope never dispatches under a fallback nobody chose — it is refused here,
// with the names it could have meant.
func TestUnit_HandleMission_UnknownEnvelopeIsRefusedWithTheList(t *testing.T) {
	disp := &fakeDispatcher{}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	tr.deps.MissionEnvelopes = fakeEnvelopes{}
	setMissionConfig(t, db, "default-mission-agent", "reviewer")

	_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"},
		"--policy hitl-policy-stcirt.json go")
	if err == nil {
		t.Fatal("an unknown envelope must be refused, not dispatched")
	}
	for _, want := range []string{"unknown mission envelope", "hitl-policy-stcirt.json", "hitl-policy-strict.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %q", want, err.Error())
		}
	}
	if disp.got.AgentName != "" {
		t.Fatalf("nothing may be dispatched after a refused envelope: %+v", disp.got)
	}
}

// TestUnit_HandleMission_UnknownConfiguredEnvelopeIsRefused pins the same check
// on the config default: a stale default-mission-policy is caught in-session
// rather than reaching a unit.
func TestUnit_HandleMission_UnknownConfiguredEnvelopeIsRefused(t *testing.T) {
	tr, db := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	tr.deps.MissionEnvelopes = fakeEnvelopes{}
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "hitl-policy-gone.json")

	_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"}, "go")
	if err == nil || !strings.Contains(err.Error(), "default-mission-policy") {
		t.Fatalf("want a refusal naming where the envelope came from, got %v", err)
	}
}

// TestUnit_ParseMissionFlags pins the grammar: flags lead, both spellings, and
// a readable refusal for anything else.
func TestUnit_ParseMissionFlags(t *testing.T) {
	cases := []struct {
		name       string
		args       string
		beta       bool
		wantPolicy string
		wantRest   string
		wantErr    string
	}{
		{name: "no flags", args: "planner do the thing", wantRest: "planner do the thing"},
		{name: "spaced value", args: "--policy p.json do it", wantPolicy: "p.json", wantRest: "do it"},
		{name: "equals value", args: "--policy=p.json do it", wantPolicy: "p.json", wantRest: "do it"},
		{name: "value missing", args: "--policy", wantErr: "--policy needs an envelope name"},
		{name: "value is another flag", args: "--policy --oracle x", wantErr: "--policy needs an envelope name"},
		{name: "unknown flag", args: "--wait now", wantErr: `unknown /mission flag "--wait"`},
		{name: "flags do not lead", args: "planner --policy p.json", wantRest: "planner --policy p.json"},
		{name: "oracle without beta is just unknown", args: "--oracle go", wantErr: `unknown /mission flag "--oracle"`},
		{name: "oracle under beta is answered exactly", args: "--oracle go", beta: true, wantErr: "OPERATOR-fired missions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, rest, err := parseMissionFlags(tc.args, tc.beta)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMissionFlags: %v", err)
			}
			if flags.policy != tc.wantPolicy {
				t.Fatalf("policy = %q, want %q", flags.policy, tc.wantPolicy)
			}
			if rest != tc.wantRest {
				t.Fatalf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestUnit_HandleMission_OracleFlagFollowsTheBetaGate pins gate parity with the
// CLI's `mission fire --oracle`: invisible with the gate off, and under the
// gate answered with why a session cannot mount the driver — never silently
// accepted.
func TestUnit_HandleMission_OracleFlagFollowsTheBetaGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		beta bool
		want string
	}{
		{name: "gate off hides it", beta: false, want: `unknown /mission flag "--oracle"`},
		{name: "gate on explains it", beta: true, want: "contenox mission fire"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &fakeDispatcher{}
			tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
			tr.deps.OptInBeta = tc.beta
			setMissionConfig(t, db, "default-mission-agent", "reviewer")
			setMissionConfig(t, db, "default-mission-policy", "envelope.json")

			_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"}, "--oracle go")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if disp.got.AgentName != "" {
				t.Fatalf("a refused flag must fire nothing: %+v", disp.got)
			}
		})
	}
}

// TestUnit_MissionStatus_MentionsOracleOnlyUnderBeta pins gate parity in the
// listing: a stable build never mentions a lever it does not have.
func TestUnit_MissionStatus_MentionsOracleOnlyUnderBeta(t *testing.T) {
	for _, beta := range []bool{false, true} {
		tr, _ := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
		tr.deps.OptInBeta = beta
		out, err := tr.handleMission(context.Background(), &sessionEntry{}, "")
		if err != nil {
			t.Fatalf("bare /mission: %v", err)
		}
		if got := strings.Contains(out, "oracle"); got != beta {
			t.Fatalf("beta=%v: oracle mentioned=%v:\n%s", beta, got, out)
		}
	}
}

func TestUnit_HandleMission_DispatchErrorSurfaces(t *testing.T) {
	disp := &fakeDispatcher{err: context.Canceled}
	tr, db := newMissionTestTransport(t, disp, &fakeResolver{})
	setMissionConfig(t, db, "default-mission-agent", "reviewer")
	setMissionConfig(t, db, "default-mission-policy", "envelope.json")
	_, err := tr.handleMission(context.Background(), &sessionEntry{InternalSessionID: "s"}, "go")
	if err == nil {
		t.Fatal("dispatch error must surface to the caller")
	}
}

// TestUnit_MissionCommandIsParsedRegardlessOfCapability pins: parseCommand recognizes "/mission" regardless of dispatch capability.
func TestUnit_MissionCommandIsParsedRegardlessOfCapability(t *testing.T) {
	name, args, ok := parseCommand("/mission reviewer do the thing")
	if !ok || name != "mission" || args != "reviewer do the thing" {
		t.Fatalf("parseCommand(/mission ...) = %q,%q,%v", name, args, ok)
	}
}

// TestUnit_AcpCommands_WithMissionCapability_IncludesMission pins: /mission is advertised whenever the fleet capability is wired.
func TestUnit_AcpCommands_WithMissionCapability_IncludesMission(t *testing.T) {
	tr, _ := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	cmds := tr.acpCommands()

	if !containsCommand(cmds, "mission") {
		t.Fatalf("mission missing from advertised commands with capability wired: %v", commandNames(cmds))
	}
	// /answer carries its own capability (Deps.Asks + Deps.Supervision), which
	// this fixture does not wire; every other command is unconditional.
	for _, c := range allACPCommands() {
		if c.Name == "answer" {
			continue
		}
		if !containsCommand(cmds, c.Name) {
			t.Fatalf("advertised commands missing %q: %v", c.Name, commandNames(cmds))
		}
	}
	if want := len(allACPCommands()) - 1; len(cmds) != want {
		t.Fatalf("advertised %d commands, want %d (full set minus answer): %v", len(cmds), want, commandNames(cmds))
	}
}

// TestUnit_AcpCommands_WithoutMissionCapability_ExcludesMission pins: /mission is dropped from the menu without the fleet capability; every other command stays.
func TestUnit_AcpCommands_WithoutMissionCapability_ExcludesMission(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)
	cmds := tr.acpCommands()

	if containsCommand(cmds, "mission") {
		t.Fatalf("mission advertised without capability: %v", commandNames(cmds))
	}
	for _, c := range allACPCommands() {
		// /answer is dropped by its own capability, not this one.
		if c.Name == "mission" || c.Name == "answer" {
			continue
		}
		if !containsCommand(cmds, c.Name) {
			t.Fatalf("advertised commands missing %q: %v", c.Name, commandNames(cmds))
		}
	}
	if want := len(allACPCommands()) - 2; len(cmds) != want {
		t.Fatalf("advertised %d commands, want %d (full set minus mission and answer): %v", len(cmds), want, commandNames(cmds))
	}
}

// TestUnit_HandleMission_TeachingErrorWithoutCapability pins: without hasMissionCapability, /mission teaches the in-process fix, never serve.
func TestUnit_HandleMission_TeachingErrorWithoutCapability(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)
	_, err := tr.handleMission(context.Background(), &sessionEntry{}, "do something")
	if err == nil {
		t.Fatal("want a teaching error, got nil")
	}
	for _, want := range []string{"unavailable", "default-model", "in-process fleet"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("teaching error missing %q: %q", want, err.Error())
		}
	}
	for _, forbidden := range []string{"Beam", "contenox serve", "serve-hosted"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("in-process teaching error must not teach serve-as-center, but contains %q: %q", forbidden, err.Error())
		}
	}
}

// TestUnit_DeliverToContenoxSession_MapsAndErrors pins: an unknown firing-session id yields ErrSessionNotLive; a bound one delivers.
func TestUnit_DeliverToContenoxSession_MapsAndErrors(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)

	err := tr.DeliverToContenoxSession(context.Background(), "nope",
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk("hi")})
	if !errors.Is(err, ErrSessionNotLive) {
		t.Fatalf("unknown contenox id must yield ErrSessionNotLive (the inbox-fallback signal), got %v", err)
	}

	tr.contenoxToACPID = map[string]libacp.SessionID{"cnx-1": "acp-1"}
	if err := tr.DeliverToContenoxSession(context.Background(), "cnx-1",
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk("hi")}); err != nil {
		t.Fatalf("a bound session must deliver (nil), got %v", err)
	}
}

func containsCommand(cmds []libacp.AvailableCommand, name string) bool {
	for _, c := range cmds {
		if c.Name == name {
			return true
		}
	}
	return false
}

func commandNames(cmds []libacp.AvailableCommand) []string {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	return names
}
