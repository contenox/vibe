package acpsvc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
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

	// The whole line is the intent; the configured default agent fires.
	if disp.got.AgentName != "reviewer" {
		t.Fatalf("agent = %q, want reviewer", disp.got.AgentName)
	}
	if disp.got.Intent != "triage the failing CI run" {
		t.Fatalf("intent = %q", disp.got.Intent)
	}
	if disp.got.HITLPolicyName != "hitl-policy-strict.json" {
		t.Fatalf("policy = %q", disp.got.HITLPolicyName)
	}
	// THE supervision edge: the firing session becomes the parent.
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

	// First token resolves to a declared agent → named form: agent = token,
	// intent = the rest.
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
	// "summarise" is not a declared agent, so the whole line is the intent.
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

// The in-process editor (Fleet+Agents wired) must confirm that reports arrive
// LIVE in this session — the ontology's supervision edge closing inside the
// editor — naming the operator inbox only as the ended-session fallback. It
// must NOT say reports go to the inbox as the primary home.
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

func TestUnit_HandleMission_EmptyArgsShowsUsage(t *testing.T) {
	tr, _ := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	_, err := tr.handleMission(context.Background(), &sessionEntry{}, "   ")
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("want usage error, got %v", err)
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

// parseCommand must recognize "/mission" unconditionally — regardless of
// whether this transport can dispatch it — so a client that sends it anyway
// (stale menu state, a remembered command) reaches handleMission's teaching
// error instead of Prompt silently forwarding "/mission ..." as chat text.
func TestUnit_MissionCommandIsParsedRegardlessOfCapability(t *testing.T) {
	name, args, ok := parseCommand("/mission reviewer do the thing")
	if !ok || name != "mission" || args != "reviewer do the thing" {
		t.Fatalf("parseCommand(/mission ...) = %q,%q,%v", name, args, ok)
	}
}

// With the fleet capability wired (Fleet + Agents both set — a serve-hosted
// session), /mission must be advertised alongside every other command: ACP is
// advertise-what-works, and here it does.
func TestUnit_AcpCommands_WithMissionCapability_IncludesMission(t *testing.T) {
	tr, _ := newMissionTestTransport(t, &fakeDispatcher{}, &fakeResolver{})
	cmds := tr.acpCommands()

	if !containsCommand(cmds, "mission") {
		t.Fatalf("mission missing from advertised commands with capability wired: %v", commandNames(cmds))
	}
	// Every other command must survive untouched — this is a filter, not a
	// wholesale swap.
	for _, c := range allACPCommands() {
		if !containsCommand(cmds, c.Name) {
			t.Fatalf("advertised commands missing %q: %v", c.Name, commandNames(cmds))
		}
	}
	if len(cmds) != len(allACPCommands()) {
		t.Fatalf("advertised %d commands, want %d (full set): %v", len(cmds), len(allACPCommands()), commandNames(cmds))
	}
}

// Without the fleet capability (the standalone `contenox acp` path — Fleet and
// Agents both nil), /mission must be dropped from the advertised menu: it is
// the one command that cannot work there, so advertising it would be dishonest
// per ACP. Every other command must still be offered, unchanged.
func TestUnit_AcpCommands_WithoutMissionCapability_ExcludesMission(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)
	cmds := tr.acpCommands()

	if containsCommand(cmds, "mission") {
		t.Fatalf("mission advertised without capability: %v", commandNames(cmds))
	}
	for _, c := range allACPCommands() {
		if c.Name == "mission" {
			continue
		}
		if !containsCommand(cmds, c.Name) {
			t.Fatalf("advertised commands missing %q: %v", c.Name, commandNames(cmds))
		}
	}
	if want := len(allACPCommands()) - 1; len(cmds) != want {
		t.Fatalf("advertised %d commands, want %d (full set minus mission): %v", len(cmds), want, commandNames(cmds))
	}
}

// handleMission's guard must be keyed to the SAME capability bit as the
// advertisement filter (hasMissionCapability), not merely Fleet==nil: a stale
// or manually-typed /mission (with no in-process fleet wired) must hit a clear
// teaching error — never a panic. The error teaches the IN-PROCESS path (a
// configured model + the editor's embedded fleet); remote forwarding is gone.
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
	// The refit forbids teaching serve as the center on the in-process path.
	for _, forbidden := range []string{"Beam", "contenox serve", "serve-hosted"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("in-process teaching error must not teach serve-as-center, but contains %q: %q", forbidden, err.Error())
		}
	}
}

// DeliverToContenoxSession is the report router's in-process reach into a live
// editor session: an unknown firing-session id is the ErrSessionNotLive signal
// (routes the report to the operator inbox), a bound one resolves and delivers.
func TestUnit_DeliverToContenoxSession_MapsAndErrors(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)

	err := tr.DeliverToContenoxSession(context.Background(), "nope",
		libacp.SessionNotification{Update: libacp.NewAgentMessageChunk("hi")})
	if !errors.Is(err, ErrSessionNotLive) {
		t.Fatalf("unknown contenox id must yield ErrSessionNotLive (the inbox-fallback signal), got %v", err)
	}

	// A bound session id resolves; conn is nil so sendUpdate is a no-op, but the
	// mapping path must return nil (delivered) rather than the not-live signal.
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
