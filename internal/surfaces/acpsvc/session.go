package acpsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libacp "github.com/contenox/contenox/libacp"
	libdb "github.com/contenox/contenox/libdbexec"
)

// sessionListTitleMaxLen bounds SessionInfo.Title derived from a session's
// first user message, so a session picker never renders a multi-paragraph prompt.
const sessionListTitleMaxLen = 60

// acpClientIdentity is the message_indices identity every ACP session is
// created under; the store reads that list sessions must filter by it.
const acpClientIdentity = "acp-client"

// truncateSessionListTitle collapses whitespace and clips to
// sessionListTitleMaxLen runes, appending an ellipsis when it clips.
func truncateSessionListTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= sessionListTitleMaxLen {
		return s
	}
	if sessionListTitleMaxLen <= 3 {
		return string(runes[:sessionListTitleMaxLen])
	}
	return string(runes[:sessionListTitleMaxLen-3]) + "..."
}

const mcpNamePrefix = runtimetypes.ACPMCPServerNamePrefix

func mcpNameFor(connectionID string, sessionID libacp.SessionID, original string) string {
	sum := sha256.Sum256([]byte(connectionID + "\x00" + string(sessionID) + "\x00" + original))
	hash := hex.EncodeToString(sum[:])[:12]
	return mcpNamePrefix + hash + "-" + sanitizeMCPNameComponent(original)
}

func sanitizeMCPNameComponent(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z':
			sb.WriteRune(r)
		case r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == '_' || r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
		if sb.Len() >= 48 {
			break
		}
	}
	out := strings.Trim(sb.String(), "_-")
	if out == "" {
		return "mcp"
	}
	return out
}

func (t *Transport) LoadSession(ctx context.Context, req libacp.LoadSessionRequest) (libacp.LoadSessionResponse, error) {
	reportErr, reportChange, end := t.tracker().Start(ctx, "load", "acp_session", "session_id", string(req.SessionID))
	defer end()

	if req.SessionID == "" {
		err := libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}
	if err := requireSessionCwd(req.Cwd); err != nil {
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}
	if t.deps.Engine == nil {
		err := errSetupRequired()
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	sessionCwd, err := t.resolveExistingSessionCwd(ctx, store, req.SessionID, req.Cwd)
	if err != nil {
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}
	workspaceID := t.workspaceID()
	if resolved, ok := t.resolveSessionWorkspace(ctx, string(req.SessionID)); ok {
		workspaceID = resolved
	}

	registered, err := t.registerMcpServers(ctx, store, req.SessionID, req.McpServers)
	if err != nil {
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}

	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})

	contenoxSessionID, messages, err := ag.SessionLoad(ctx, string(req.SessionID))
	if err != nil {
		t.cleanupMcpServers(ctx, store, registered)
		wrapped := libacp.NewErrorf(libacp.ErrInvalidParams, "load session %q: %v", req.SessionID, err)
		reportErr(wrapped)
		return libacp.LoadSessionResponse{}, wrapped
	}

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		McpServerNames:    registered,
		driver:            &nativeDriver{t: t, agent: ag},
		Provider:          t.provider(),
		Model:             t.model(),
		Think:             t.thinkDefault(),
		HITLPolicy:        hitlPolicyDefaultValue,
	}
	t.sessionMu.Lock()
	t.sessions[req.SessionID] = entry
	t.bindContenoxSession(contenoxSessionID, req.SessionID)
	t.sessionMu.Unlock()
	t.persistSessionCwd(ctx, store, req.SessionID, sessionCwd)
	// Re-flag an external session from its persisted agent name; the
	// downstream process is lazily respawned on the next prompt, not here.
	t.markExternalIfPersisted(ctx, store, req.SessionID, entry)
	// A reloaded session that fired missions is still their supervisor.
	entry.FiredMissions = t.readSessionFiredMission(ctx, store, req.SessionID)
	// And a reloaded session that IS a mission still holds its mission tools.
	t.restoreSessionMission(ctx, store, req.SessionID, entry)

	t.clearToolCallState(req.SessionID)
	t.subscribeTerminal(req.SessionID, contenoxSessionID)
	// The flag reflects how tool messages were persisted: external stored each
	// tool call as a self-contained ACP record, native stored assistant
	// CallTools + tool-result text.
	_, isExternal := entry.driver.(*externalDriver)
	t.replayMessages(ctx, req.SessionID, messages, isExternal)
	// Join an in-flight native turn as a viewer so the client resumes the
	// live stream; ordered after the replay, which the journal never overlaps.
	t.reattachNativeTurn(ctx, req.SessionID)
	t.reofferParkedAsks(ctx, req.SessionID, contenoxSessionID)
	// Emit the slash-command menu only after the session/load result is on
	// the wire. External re-emits its downstream agent's persisted menu (the
	// live bridge died with the pre-load connection).
	if _, isExternal := entry.driver.(*externalDriver); isExternal {
		t.reemitExternalCommandMenu(ctx, store, req.SessionID)
	} else if entry.driver.AvailableCommands() != nil {
		libacp.AfterResponse(ctx, func() {
			t.sendAvailableCommands(ctx, req.SessionID)
			if banner := t.takeBanner(); banner != "" {
				t.sendUpdate(ctx, libacp.SessionNotification{
					SessionID: req.SessionID,
					Update:    libacp.NewAgentMessageChunk(banner),
				})
			}
		})
	}

	reportChange(string(req.SessionID), map[string]any{
		"contenox_session_id": contenoxSessionID,
		"message_count":       len(messages),
	})
	return libacp.LoadSessionResponse{ConfigOptions: t.reloadedConfigOptions(ctx, store, req.SessionID, entry)}, nil
}

func (t *Transport) replayMessages(ctx context.Context, sessionID libacp.SessionID, messages []taskengine.Message, external bool) {
	_, reportChange, end := t.tracker().Start(ctx, "replay", "acp_session", "session_id", string(sessionID), "message_count", len(messages))
	defer end()

	// A call's outcome lives only in its later result message, so status must
	// be resolved in a pre-pass — otherwise every replayed tool_call shows
	// "completed" regardless of what actually happened.
	statuses := replayToolStatuses(messages, external)

	var users, assistantText, toolCalls, toolResults, failedTools int
	// One messageId per historical message: the spec groups replayed chunks by
	// id, so thinking + text of one assistant turn render as one message.
	for i, m := range messages {
		messageID := fmt.Sprintf("replay-%d", i)
		switch m.Role {
		case "user":
			if m.Content == "" {
				continue
			}
			update := libacp.NewUserMessageChunk(m.Content)
			update.MessageID = messageID
			t.sendUpdateLocal(ctx, libacp.SessionNotification{
				SessionID: sessionID,
				Update:    update,
			})
			users++
		case "assistant":
			if m.Thinking != "" {
				update := libacp.NewAgentThoughtChunk(m.Thinking)
				update.MessageID = messageID
				t.sendUpdateLocal(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    update,
				})
			}
			if m.Content != "" {
				update := libacp.NewAgentMessageChunk(m.Content)
				update.MessageID = messageID
				t.sendUpdateLocal(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    update,
				})
				assistantText++
			}
			for _, tc := range m.CallTools {
				status := replayStatusFor(statuses, tc.ID)
				t.sendUpdateLocal(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    toolCallUpdateFromCall(tc, status),
				})
				toolCalls++
				if status == libacp.ToolCallStatusFailed {
					failedTools++
				}
			}
		case "tool":
			// External carries a self-contained ACP tool record; native is raw
			// result text paired with the assistant's CallTools above.
			update := toolCallUpdateFromResult(m)
			if external {
				if u, ok := externalToolReplayUpdate(m); ok {
					update = u
				}
			}
			t.sendUpdateLocal(ctx, libacp.SessionNotification{
				SessionID: sessionID,
				Update:    update,
			})
			toolResults++
		}
	}
	reportChange(string(sessionID), map[string]any{
		"user":         users,
		"assistant":    assistantText,
		"tool_calls":   toolCalls,
		"tool_results": toolResults,
		"failed_tools": failedTools,
	})

	// A loaded session must carry the used half too, or the gauge reads
	// "0 of N used" over a full history until the next turn corrects it.
	t.sendUsageUpdate(ctx, sessionID, estimateHistoryTokens(messages))
}

// replayToolStatuses maps each replayed tool call id to the terminal status
// its persisted result records. External is exact (the stored ACP record
// carries a status field); native is recovered from the two shapes the engine
// writes for a failure — taskexec's "tool <name> execution failed: <err>"
// string and toolErrorContent's single-key {"error": ...} object — which
// couples to the engine's error-formatting and breaks silently if it changes.
func replayToolStatuses(messages []taskengine.Message, external bool) map[string]libacp.ToolCallStatus {
	out := make(map[string]libacp.ToolCallStatus)
	for _, m := range messages {
		if m.Role != "tool" {
			continue
		}
		id := m.ToolCallID
		status := replayToolStatus(m.Content)
		if external {
			if u, ok := externalToolReplayUpdate(m); ok {
				id, status = u.ToolCallID, u.Status
			}
		}
		if id == "" {
			continue
		}
		out[id] = status
	}
	return out
}

// replayStatusFor resolves the status to replay a tool call with. A call with
// no result message in the transcript keeps "completed": no recorded outcome
// is not evidence of failure, and a red cross would be a louder lie.
func replayStatusFor(statuses map[string]libacp.ToolCallStatus, toolCallID string) libacp.ToolCallStatus {
	if status, ok := statuses[toolCallID]; ok && status != "" {
		return status
	}
	return libacp.ToolCallStatusCompleted
}

// toolExecFailedRE matches the sentence taskengine substitutes for a tool
// result when the tool returned an error. Anchored to a single unspaced tool
// name so ordinary tool output discussing a failure isn't mistaken for one.
var toolExecFailedRE = regexp.MustCompile(`^tool [^\s]+ execution failed: `)

// replayToolStatus derives one native tool call's terminal status from its
// persisted result content. The {"error": ...} test requires exactly that one
// key: a result carrying an error field alongside other fields (local_shell's
// `error` beside `exit_code`) is a successful call reporting what it found
// and must replay as completed, agreeing with the live path.
func replayToolStatus(content string) libacp.ToolCallStatus {
	s := strings.TrimSpace(content)
	if s == "" {
		return libacp.ToolCallStatusCompleted
	}
	// The engine serializes a tool result through json.Marshal on every error
	// path, so the failure sentence arrives as a JSON string literal.
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal([]byte(s), &unquoted); err == nil {
			s = strings.TrimSpace(unquoted)
		}
	}
	// A persisted policy denial replays as failed, matching the live wire.
	if _, denied := policyDenialReason(s); denied {
		return libacp.ToolCallStatusFailed
	}
	if strings.HasPrefix(s, "{") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &obj); err == nil && len(obj) == 1 {
			if raw, ok := obj["error"]; ok {
				var msg string
				if json.Unmarshal(raw, &msg) == nil && strings.TrimSpace(msg) != "" {
					return libacp.ToolCallStatusFailed
				}
			}
		}
	}
	if toolExecFailedRE.MatchString(s) {
		return libacp.ToolCallStatusFailed
	}
	return libacp.ToolCallStatusCompleted
}

// estimateHistoryTokens estimates the "used" half of a reloaded session's
// usage_update from its transcript, using the same arithmetic as the engine's
// tokenizer (runes/4, floor 1 per non-empty string). It under-counts by the
// system prelude and tool-schema JSON, and snaps to exact on the next
// token_usage event; deliberately not corrected by a guessed constant.
func estimateHistoryTokens(messages []taskengine.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateContentTokens(m.Content)
	}
	return total
}

// estimateContentTokens mirrors ollamatokenizer.EstimateTokenizer.CountTokens.
func estimateContentTokens(s string) int {
	r := utf8.RuneCountInString(s)
	if r == 0 {
		return 0
	}
	if n := r / 4; n >= 1 {
		return n
	}
	return 1
}

// toolCallUpdateFromCall renders one replayed tool call from the assistant
// message that opened it; status comes from the transcript's later
// tool-result message (see replayToolStatuses).
func toolCallUpdateFromCall(tc taskengine.ToolCall, status libacp.ToolCallStatus) libacp.SessionUpdate {
	title := tc.Function.Name
	var argsMap map[string]any
	if tc.Function.Arguments != "" && json.Valid([]byte(tc.Function.Arguments)) {
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &argsMap)
	}
	if summary := summarizeToolCallArgs(tc.Function.Name, argsMap); summary != "" {
		title = tc.Function.Name + ": " + summary
	}
	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCall,
		ToolCallID:    tc.ID,
		Title:         title,
		Kind:          toolKindFor(tc.Function.Name),
		Status:        status,
	}
	if tc.Function.Arguments != "" && json.Valid([]byte(tc.Function.Arguments)) {
		update.RawInput = json.RawMessage(tc.Function.Arguments)
	}
	return update
}

// externalToolReplayUpdate decodes a persisted externalToolRecord into the
// tool_call update that reconstructs its card verbatim; false means not an
// external record, and the caller falls back to the native result mapping.
func externalToolReplayUpdate(m taskengine.Message) (libacp.SessionUpdate, bool) {
	var rec externalToolRecord
	if err := json.Unmarshal([]byte(m.Content), &rec); err != nil {
		return libacp.SessionUpdate{}, false
	}
	toolCallID := rec.ToolCallID
	if toolCallID == "" {
		toolCallID = m.ToolCallID
	}
	if toolCallID == "" {
		return libacp.SessionUpdate{}, false
	}
	status := rec.Status
	if status == "" {
		status = libacp.ToolCallStatusCompleted
	}
	title := rec.Title
	if title == "" {
		// The spec requires a title; fall back to the id for strict clients.
		title = toolCallID
	}
	return libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCall,
		ToolCallID:    toolCallID,
		Title:         title,
		Kind:          rec.Kind,
		Status:        status,
		RawInput:      rec.RawInput,
		RawOutput:     rec.RawOutput,
		ToolContent:   rec.ToolContent,
		Locations:     rec.Locations,
	}, true
}

// toolCallUpdateFromResult renders the closing update of one replayed native
// tool call, deriving status from the result content — the same derivation
// replayToolStatuses applies to the opening card, so both halves agree.
func toolCallUpdateFromResult(m taskengine.Message) libacp.SessionUpdate {
	update := libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateToolCallUpdate,
		ToolCallID:    m.ToolCallID,
		Status:        replayToolStatus(m.Content),
	}
	if m.Content != "" {
		update.RawOutput = json.RawMessage(jsonString(m.Content))
		if diff := diffContentFromResult(m.Content); diff != nil {
			update.ToolContent = []libacp.ToolCallContent{*diff}
		}
	}
	return update
}

// errSetupRequired is returned by session operations when running setup-only
// (no default-model, engine nil). The code is the spec's -32000 auth_required,
// so a conformant client offers the advertised auth methods — the setup flow.
func errSetupRequired() error {
	return libacp.NewError(libacp.ErrAuthRequired, "contenox is not configured yet: no default-model is set. Run the \"Setup Contenox\" auth method, set the CONTENOX_DEFAULT_* environment variables (or run `contenox acp --setup`), then reconnect.")
}

// requireSessionCwd rejects a malformed session cwd on session/new,
// session/load and session/resume before any of them touches the engine or
// the store, so a bad request costs no side effects.
//
// Only one rule is this transport's own: the ACP spec makes cwd mandatory, so
// an absent one is a client bug rather than an unspecified workspace. The path
// rules are not restated here — the check runs vfs.ResolveSessionCwd against a
// nil allowlist, which applies exactly its syntactic half (absolute, and never
// inside the runtime's control plane) and leaves the allowlist decision to the
// real resolution later. One procedure, one implementation.
//
// The "/" sentinel deliberately survives. It means "the machine's default
// root", it is what every browser client sends, and the duplicated
// filepath.IsAbs check this replaced refused it before the allowlist was ever
// consulted — which on Windows, where "/" is not an absolute path, refused
// every remote session outright.
func requireSessionCwd(cwd string) error {
	if strings.TrimSpace(cwd) == "" {
		return libacp.NewError(libacp.ErrInvalidParams, "cwd is required")
	}
	if _, err := vfs.ResolveSessionCwd(nil, cwd, ""); err != nil {
		return libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	return nil
}

// resolveWorkspaceCwd maps a requested session cwd onto the concrete root to
// use, delegating the decision to vfs.ResolveSessionCwd (shared with
// fleetservice's dispatch path) and translating a refusal into a wire error.
// This is the enforcement point for the workspace-root allowlist: a cwd a
// client proposes is untrusted input, and when Deps.WorkspaceRoots is
// configured anything outside it is refused rather than adopted.
// See vfs.ResolveSessionCwd for the full rule set.
func (t *Transport) resolveWorkspaceCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(t.deps.WorkspaceRoots, cwd, "")
	if err != nil {
		return "", libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	return resolved, nil
}

// resolveExistingSessionCwd is resolveWorkspaceCwd plus one extra rule: the
// sentinel "/" or empty cwd sent on every load/resume must not clobber the
// session's stored workspace back to the default, so the persisted cwd wins
// when still allowlisted.
func (t *Transport) resolveExistingSessionCwd(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, cwd string) (string, error) {
	if f := t.deps.WorkspaceRoots; f != nil && (cwd == "" || cwd == "/") {
		if existing := t.sessionCwd(ctx, store, sid); existing != "" {
			if resolved, ok := f.Allows(existing); ok {
				return resolved, nil
			}
		}
	}
	return t.resolveWorkspaceCwd(cwd)
}

func (t *Transport) NewSession(ctx context.Context, req libacp.NewSessionRequest) (libacp.NewSessionResponse, error) {
	internalID := newSessionID(sessionNamespace(t))
	sessionID := libacp.SessionID(internalID)

	reportErr, reportChange, end := t.tracker().Start(ctx, "new", "acp_session", "session_id", string(sessionID), "cwd", req.Cwd, "mcp_servers", len(req.McpServers))
	defer end()

	if err := requireSessionCwd(req.Cwd); err != nil {
		reportErr(err)
		return libacp.NewSessionResponse{}, err
	}
	if t.deps.Engine == nil {
		err := errSetupRequired()
		reportErr(err)
		return libacp.NewSessionResponse{}, err
	}
	sessionCwd, err := t.resolveWorkspaceCwd(req.Cwd)
	if err != nil {
		reportErr(err)
		return libacp.NewSessionResponse{}, err
	}

	workspaceID := t.workspaceID()

	store := runtimetypes.New(t.deps.DB.WithoutTransaction())

	// contenox.adopt `_meta` adopts an already-running instance+session,
	// taking precedence over contenox.agent (nothing is spawned).
	// Absent/malformed falls through unchanged.
	if ref, ok := parseAdoptMeta(req.Meta); ok {
		resp, adoptErr := t.newAdoptedSession(ctx, internalID, sessionID, sessionCwd, workspaceID, store, ref, reportChange)
		if adoptErr != nil {
			reportErr(adoptErr)
			return libacp.NewSessionResponse{}, adoptErr
		}
		return resp, nil
	}

	// A client binds this session to a registered external ACP agent via the
	// contenox.agent `_meta` key; absent, the native chain path below runs.
	if agentName := parseAgentMeta(req.Meta); agentName != "" {
		// Bring up the downstream agent first: any spawn/handshake failure
		// must fail session/new cleanly with no session and no leaked process.
		att, spawnErr := t.bringUpExternal(ctx, sessionID, sessionCwd, agentName, false)
		if spawnErr != nil {
			reportErr(spawnErr)
			return libacp.NewSessionResponse{}, spawnErr
		}
		bridge := att.bridge
		ag := agentservice.New(agentservice.Deps{
			Engine:      t.deps.Engine,
			DB:          t.deps.DB,
			WorkspaceID: workspaceID,
			Identity:    "acp-client",
		})
		contenoxSessionID, sessErr := ag.SessionNew(ctx, internalID)
		if sessErr != nil {
			att.teardown(t)
			wrapped := fmt.Errorf("could not start a session: %w", sessErr)
			reportErr(wrapped)
			return libacp.NewSessionResponse{}, wrapped
		}
		entry := &sessionEntry{
			WorkspaceID:       workspaceID,
			Cwd:               sessionCwd,
			InternalSessionID: contenoxSessionID,
			HITLPolicy:        hitlPolicyDefaultValue,
			driver: &externalDriver{
				t:            t,
				agentName:    agentName,
				upstreamID:   sessionID,
				conn:         att.conn,
				handle:       att.handle,
				instanceID:   att.instanceID,
				downstreamID: att.downstreamID,
				bridge:       bridge,
			},
		}
		t.sessionMu.Lock()
		t.sessions[sessionID] = entry
		t.bindContenoxSession(contenoxSessionID, sessionID)
		t.sessionMu.Unlock()
		t.persistSessionCwd(ctx, store, sessionID, sessionCwd)
		t.persistSessionAgent(ctx, store, sessionID, agentName)
		// Persist the instance and downstream session ids so a later
		// session/load re-attaches to this same running instance.
		t.persistSessionInstance(ctx, sessionID, att.instanceID)
		t.persistSessionDownstream(ctx, sessionID, att.downstreamID)
		t.clearToolCallState(sessionID)

		// The downstream menu was cached (it references a session id the
		// client hasn't learned yet); re-emit once the result is on the wire.
		libacp.AfterResponse(ctx, func() {
			bridge.markBound(ctx)
		})

		reportChange(string(sessionID), map[string]any{
			"contenox_session_id": contenoxSessionID,
			"workspace_id":        workspaceID,
			"external_agent":      agentName,
		})
		return libacp.NewSessionResponse{
			SessionID:     sessionID,
			ConfigOptions: t.sessionConfigOptions(ctx, entry),
			Meta:          agentMetaJSON(entry.driver.AgentName()),
		}, nil
	}

	registered, err := t.registerMcpServers(ctx, store, sessionID, req.McpServers)
	if err != nil {
		reportErr(err)
		return libacp.NewSessionResponse{}, err
	}

	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})

	contenoxSessionID, err := ag.SessionNew(ctx, internalID)
	if err != nil {
		t.cleanupMcpServers(ctx, store, registered)
		wrapped := fmt.Errorf("could not start a session: %w", err)
		reportErr(wrapped)
		return libacp.NewSessionResponse{}, wrapped
	}

	// A dispatched unit's session/new carries its mission id and compute
	// allowlists in `_meta`, scoping mission tools and model resolution to
	// that mission (see missionservice.MissionMeta). Absent/empty for an
	// ordinary chat session or an unbounded mission.
	missionMeta, _ := missionservice.ParseMissionMetaFull(req.Meta)
	missionID := missionMeta.MissionID

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		McpServerNames:    registered,
		MissionID:         missionID,
		ModelAllowlist:    missionMeta.ModelAllowlist,
		BackendAllowlist:  missionMeta.BackendAllowlist,
		driver:            &nativeDriver{t: t, agent: ag},
		Provider:          t.provider(),
		Model:             t.model(),
		Think:             t.thinkDefault(),
		// A unit is gated by the envelope its mission was fired under, not by its host's policy.
		HITLPolicy: missionHITLPolicy(missionMeta.HITLPolicyName),
	}
	t.sessionMu.Lock()
	t.sessions[sessionID] = entry
	t.bindContenoxSession(contenoxSessionID, sessionID)
	t.sessionMu.Unlock()
	t.persistSessionCwd(ctx, store, sessionID, sessionCwd)
	t.persistSessionMission(ctx, store, sessionID, missionMeta)
	t.clearToolCallState(sessionID)
	t.subscribeTerminal(sessionID, contenoxSessionID)

	// An available_commands_update sent before the session/new result is
	// dropped as unknown; defer until libacp has written the result.
	libacp.AfterResponse(ctx, func() {
		t.sendAvailableCommands(ctx, sessionID)
		if banner := t.takeBanner(); banner != "" {
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sessionID,
				Update:    libacp.NewAgentMessageChunk(banner),
			})
		}
		t.sendInitialUsageUpdate(ctx, sessionID)
	})

	reportChange(string(sessionID), map[string]any{
		"contenox_session_id": contenoxSessionID,
		"workspace_id":        workspaceID,
	})
	return libacp.NewSessionResponse{
		SessionID:     sessionID,
		ConfigOptions: t.sessionConfigOptions(ctx, entry),
	}, nil
}

// ResumeSession is session/load without the history replay: the client kept
// its transcript and only needs the server-side session re-bound.
func (t *Transport) ResumeSession(ctx context.Context, req libacp.ResumeSessionRequest) (libacp.ResumeSessionResponse, error) {
	reportErr, reportChange, end := t.tracker().Start(ctx, "resume", "acp_session", "session_id", string(req.SessionID))
	defer end()

	if req.SessionID == "" {
		err := libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}
	if err := requireSessionCwd(req.Cwd); err != nil {
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}
	if t.deps.Engine == nil {
		err := errSetupRequired()
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	sessionCwd, err := t.resolveExistingSessionCwd(ctx, store, req.SessionID, req.Cwd)
	if err != nil {
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}
	workspaceID := t.workspaceID()
	if resolved, ok := t.resolveSessionWorkspace(ctx, string(req.SessionID)); ok {
		workspaceID = resolved
	}

	registered, err := t.registerMcpServers(ctx, store, req.SessionID, req.McpServers)
	if err != nil {
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}

	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})

	contenoxSessionID, err := ag.SessionResume(ctx, string(req.SessionID))
	if err != nil {
		t.cleanupMcpServers(ctx, store, registered)
		wrapped := libacp.NewErrorf(libacp.ErrInvalidParams, "resume session %q: %v", req.SessionID, err)
		reportErr(wrapped)
		return libacp.ResumeSessionResponse{}, wrapped
	}

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		McpServerNames:    registered,
		driver:            &nativeDriver{t: t, agent: ag},
		Provider:          t.provider(),
		Model:             t.model(),
		Think:             t.thinkDefault(),
		HITLPolicy:        hitlPolicyDefaultValue,
	}
	t.sessionMu.Lock()
	t.sessions[req.SessionID] = entry
	t.bindContenoxSession(contenoxSessionID, req.SessionID)
	t.sessionMu.Unlock()
	t.persistSessionCwd(ctx, store, req.SessionID, sessionCwd)
	t.markExternalIfPersisted(ctx, store, req.SessionID, entry)
	// Mirror LoadSession: a resumed session keeps both halves of its mission
	// relationship — the units it fired, and the mission it is.
	entry.FiredMissions = t.readSessionFiredMission(ctx, store, req.SessionID)
	t.restoreSessionMission(ctx, store, req.SessionID, entry)
	t.clearToolCallState(req.SessionID)
	t.subscribeTerminal(req.SessionID, contenoxSessionID)
	// Join an in-flight native turn so the resumed session picks the live
	// stream back up; a no-op when no turn is in flight (see LoadSession).
	t.reattachNativeTurn(ctx, req.SessionID)
	t.reofferParkedAsks(ctx, req.SessionID, contenoxSessionID)

	// Mirror LoadSession: native re-advertises its menu; external re-emits
	// its downstream agent's persisted menu.
	if _, isExternal := entry.driver.(*externalDriver); isExternal {
		t.reemitExternalCommandMenu(ctx, store, req.SessionID)
	} else if entry.driver.AvailableCommands() != nil {
		libacp.AfterResponse(ctx, func() {
			t.sendAvailableCommands(ctx, req.SessionID)
		})
	}

	// Resume keeps the client's transcript but not its gauge, so the history
	// is read here rather than replayed. Pushed after the result: resume's
	// contract is that nothing precedes its response (unlike load).
	libacp.AfterResponse(ctx, func() {
		t.sendResumedUsageUpdate(ctx, req.SessionID, entry)
	})

	reportChange(string(req.SessionID), map[string]any{
		"contenox_session_id": contenoxSessionID,
	})
	return libacp.ResumeSessionResponse{ConfigOptions: t.reloadedConfigOptions(ctx, store, req.SessionID, entry)}, nil
}

// SetSessionMode is not supported: the equivalent controls are session
// config options. Initialize never returns a Modes state, so a conformant
// client never calls this.
func (t *Transport) SetSessionMode(_ context.Context, _ libacp.SetSessionModeRequest) (libacp.SetSessionModeResponse, error) {
	return libacp.SetSessionModeResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetMode)
}

// SetSessionModel is not supported: the runtime never advertises a `models`
// state. An external session's model picker is surfaced as the synthetic
// AgentModelConfigOptionID config option instead.
func (t *Transport) SetSessionModel(_ context.Context, _ libacp.SetSessionModelRequest) (libacp.SetSessionModelResponse, error) {
	return libacp.SetSessionModelResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetModel)
}

// CloseSession releases a session's connection-local resources without
// touching its stored history; closing an unknown session succeeds. Close
// detaches, delete stops: the kernel's per-session state is deliberately left
// behind so a reconnect can re-attach with the downstream agent's context and
// fleetservice.Cancel / resolveAdoptTarget can still reach the session. The
// retention is bounded and fully reclaimed when DeleteSession stops the instance.
func (t *Transport) CloseSession(ctx context.Context, req libacp.CloseSessionRequest) (libacp.CloseSessionResponse, error) {
	_, reportChange, end := t.tracker().Start(ctx, "close", "acp_session", "session_id", string(req.SessionID))
	defer end()

	if req.SessionID == "" {
		return libacp.CloseSessionResponse{}, libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
	}
	entry := t.dropSessionEntry(req.SessionID)
	if entry != nil && t.deps.DB != nil {
		store := runtimetypes.New(t.deps.DB.WithoutTransaction())
		t.cleanupMcpServers(ctx, store, entry.McpServerNames)
	}
	// Tear down the driver now rather than waiting for connection teardown to
	// reap it (external closes its downstream agent; native is a no-op).
	if entry != nil {
		_ = entry.driver.Close()
	}
	t.clearToolCallState(req.SessionID)
	// Unlike a bare connection drop, which keeps the shell alive for reconnect,
	// an explicit close tears it down (the idle reaper handles abandonment).
	t.closeTerminal(req.SessionID, entry)
	reportChange(string(req.SessionID), map[string]any{"was_open": entry != nil})
	return libacp.CloseSessionResponse{}, nil
}

// DeleteSession removes the session's stored history and connection-local
// state. Per spec, deleting a nonexistent session succeeds silently.
func (t *Transport) DeleteSession(ctx context.Context, req libacp.DeleteSessionRequest) (libacp.DeleteSessionResponse, error) {
	reportErr, reportChange, end := t.tracker().Start(ctx, "delete", "acp_session", "session_id", string(req.SessionID))
	defer end()

	if req.SessionID == "" {
		err := libacp.NewError(libacp.ErrInvalidParams, "sessionId is required")
		reportErr(err)
		return libacp.DeleteSessionResponse{}, err
	}
	if t.deps.Engine == nil {
		err := errSetupRequired()
		reportErr(err)
		return libacp.DeleteSessionResponse{}, err
	}

	workspaceID := t.workspaceID()
	if resolved, ok := t.resolveSessionWorkspace(ctx, string(req.SessionID)); ok {
		workspaceID = resolved
	}

	entry := t.dropSessionEntry(req.SessionID)
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())
	if entry != nil {
		t.cleanupMcpServers(ctx, store, entry.McpServerNames)
	}
	// Unlike close, which only detaches, a delete must terminate the instance
	// outright; entry.driver.Close() alone leaves a Manager-owned instance Running.
	if entry != nil {
		_ = entry.driver.Close()
	}
	// Stop the Manager-owned instance from the persisted instanceID (the
	// durable source of truth, since entry may be nil if not open here).
	if t.deps.Instances != nil {
		if instanceID := t.readSessionInstance(ctx, store, req.SessionID); instanceID != "" {
			_ = t.deps.Instances.Stop(instanceID)
		}
	}
	// Unlike a plain close/disconnect, which keeps a native turn alive for
	// reconnect, a delete terminates it.
	if t.deps.NativeTurns != nil {
		t.deps.NativeTurns.Cancel(req.SessionID)
	}
	t.clearToolCallState(req.SessionID)
	t.closeTerminal(req.SessionID, entry)

	ag := agentservice.New(agentservice.Deps{
		Engine:      t.deps.Engine,
		DB:          t.deps.DB,
		WorkspaceID: workspaceID,
		Identity:    "acp-client",
	})
	if err := ag.SessionDelete(ctx, string(req.SessionID)); err != nil {
		reportErr(err)
		return libacp.DeleteSessionResponse{}, libacp.InternalError(err.Error())
	}
	_ = store.DeleteKV(ctx, acpSessionCwdKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionAgentKVPrefix+string(req.SessionID))
	// These keys are meaningful only alongside the agent-name key; drop them with it.
	_ = store.DeleteKV(ctx, acpSessionAgentCommandsKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionAgentConfigOptionsKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionHITLPolicyKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionInstanceKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionDownstreamKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionMissionKVPrefix+string(req.SessionID))

	reportChange(string(req.SessionID), map[string]any{"was_open": entry != nil})
	return libacp.DeleteSessionResponse{}, nil
}

// dropSessionEntry removes a session from the in-memory maps and returns the
// removed entry (nil if it was not open on this connection).
func (t *Transport) dropSessionEntry(sid libacp.SessionID) *sessionEntry {
	// Abort any in-flight turn before this connection-local state goes away,
	// so a racing prompt doesn't keep running against a dropped session.
	t.cancelInflightPrompt(sid)
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	entry, ok := t.sessions[sid]
	if !ok {
		return nil
	}
	delete(t.sessions, sid)
	t.unbindContenoxSession(entry.InternalSessionID)
	return entry
}

func (t *Transport) registerMcpServers(ctx context.Context, store runtimetypes.Store, sessionID libacp.SessionID, servers []libacp.McpServer) ([]string, error) {
	var registered []string
	for _, srv := range servers {
		if err := srv.Validate(); err != nil {
			t.cleanupMcpServers(ctx, store, registered)
			return nil, fmt.Errorf("invalid MCP server %q: %w", srv.Name, err)
		}
		name := mcpNameFor(t.mcpOwnerID(), sessionID, srv.Name)
		row := mcpRowFromLibacp(name, srv)
		if err := store.UpsertMCPServerByName(ctx, row); err != nil {
			t.cleanupMcpServers(ctx, store, registered)
			return nil, fmt.Errorf("could not register MCP server %q: %w", srv.Name, err)
		}
		if t.deps.Engine != nil && t.deps.Engine.MCPManager != nil {
			if err := t.deps.Engine.MCPManager.StartWorker(ctx, row); err != nil {
				registered = append(registered, name)
				t.cleanupMcpServers(ctx, store, registered)
				return nil, fmt.Errorf("could not start MCP server %q: %w", srv.Name, err)
			}
		}
		registered = append(registered, name)
	}
	return registered, nil
}

func (t *Transport) cleanupMcpServers(ctx context.Context, store runtimetypes.Store, names []string) {
	for _, name := range names {
		if t.deps.Engine != nil && t.deps.Engine.MCPManager != nil {
			t.deps.Engine.MCPManager.StopWorker(ctx, name)
		}
		cleanupMCPSessionIDs(ctx, store, name)
		row, err := store.GetMCPServerByName(ctx, name)
		if err != nil {
			if errors.Is(err, libdb.ErrNotFound) {
				continue
			}
			continue
		}
		_ = store.DeleteMCPServer(ctx, row.ID)
	}
}

func cleanupMCPSessionIDs(ctx context.Context, store runtimetypes.Store, serverName string) {
	prefix := "mcp_session:" + serverName + ":"
	for {
		page, err := store.ListKVPrefix(ctx, prefix, nil, 100)
		if err != nil {
			return
		}
		for _, kv := range page {
			_ = store.DeleteKV(ctx, kv.Key)
		}
		if len(page) < 100 {
			return
		}
	}
}

func (t *Transport) runtimeToolsAllowlist(ctx context.Context, store runtimetypes.Store, sessionNames []string) ([]string, error) {
	allowlist := []string{"*"}
	current := make(map[string]struct{}, len(sessionNames))
	for _, name := range sessionNames {
		current[name] = struct{}{}
	}
	var cursor *time.Time
	for {
		page, err := store.ListMCPServers(ctx, cursor, 100)
		if err != nil {
			return nil, fmt.Errorf("could not read this session's MCP servers: %w", err)
		}
		for _, srv := range page {
			if !runtimetypes.IsACPManagedMCPServerName(srv.Name) {
				continue
			}
			if _, ok := current[srv.Name]; ok {
				continue
			}
			allowlist = append(allowlist, "!"+srv.Name)
		}
		if len(page) < 100 {
			return allowlist, nil
		}
		cursor = &page[len(page)-1].CreatedAt
	}
}

// CleanupStaleACPManagedMCPServers removes client-scoped ACP MCP registrations
// left behind by a previous process. Durable MCP configuration must be created
// through the normal `contenox mcp` commands or HTTP API; session/new and
// session/load MCP servers are temporary by ACP contract.
func CleanupStaleACPManagedMCPServers(ctx context.Context, db libdb.DBManager) error {
	if db == nil {
		return nil
	}
	store := runtimetypes.New(db.WithoutTransaction())
	var stale []*runtimetypes.MCPServer
	var cursor *time.Time
	for {
		page, err := store.ListMCPServers(ctx, cursor, 100)
		if err != nil {
			return err
		}
		for _, srv := range page {
			if runtimetypes.IsACPManagedMCPServerName(srv.Name) {
				stale = append(stale, srv)
			}
		}
		if len(page) < 100 {
			break
		}
		cursor = &page[len(page)-1].CreatedAt
	}
	for _, srv := range stale {
		cleanupMCPSessionIDs(ctx, store, srv.Name)
		if err := store.DeleteMCPServer(ctx, srv.ID); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (t *Transport) mcpOwnerID() string {
	if t.connectionID != "" {
		return t.connectionID
	}
	return "conn-unknown"
}

func mcpRowFromLibacp(name string, srv libacp.McpServer) *runtimetypes.MCPServer {
	row := &runtimetypes.MCPServer{
		Name:                  name,
		Transport:             "stdio",
		Command:               srv.Command,
		Args:                  srv.Args,
		URL:                   srv.URL,
		ConnectTimeoutSeconds: 30,
	}
	switch srv.Kind() {
	case libacp.McpServerKindHTTP:
		row.Transport = "http"
	case libacp.McpServerKindSSE:
		row.Transport = "sse"
	default:
		row.Transport = "stdio"
	}
	if len(srv.Headers) > 0 {
		row.Headers = make(map[string]string, len(srv.Headers))
		for _, h := range srv.Headers {
			row.Headers[h.Name] = h.Value
		}
	}
	return row
}

func newSessionID(namespace string) string {
	return namespace + "-" + uuid.NewString()
}

func sessionNamespace(t *Transport) string {
	id := t.clientIdentity()
	if id == nil {
		return "acp"
	}
	var sb strings.Builder
	for _, r := range strings.ToLower(id.Name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
		if sb.Len() >= 16 {
			break
		}
	}
	if sb.Len() == 0 {
		return "acp"
	}
	return sb.String()
}

func (t *Transport) Close(ctx context.Context) error {
	store := runtimetypes.New(t.deps.DB.WithoutTransaction())

	// A bare connection drop stops streaming but does not kill shells: a
	// reload reconnects and re-subscribes. Idle-timeout reclaims the rest.
	t.unsubscribeAllTerminals()

	t.sessionMu.Lock()
	entries := make([]*sessionEntry, 0, len(t.sessions))
	sids := make([]libacp.SessionID, 0, len(t.sessions))
	for sid, e := range t.sessions {
		entries = append(entries, e)
		sids = append(sids, sid)
		// Deregister from the shared router so approvals stop routing here.
		t.deps.SessionRouter.unbind(e.InternalSessionID, t)
	}
	t.sessions = make(map[libacp.SessionID]*sessionEntry)
	t.contenoxToACPID = make(map[string]libacp.SessionID)
	t.sessionMu.Unlock()

	// The server owns cancellation here too, rather than depending solely on
	// libacp cancelling the prompt contexts it substituted when Run ends.
	for _, sid := range sids {
		t.cancelInflightPrompt(sid)
	}

	for _, e := range entries {
		t.cleanupMcpServers(ctx, store, e.McpServerNames)
		// External closes its downstream agent (idempotent with the
		// connCtx-cancel teardown in New()); native is a no-op.
		_ = e.driver.Close()
	}
	return nil
}

func (t *Transport) sessionFor(id libacp.SessionID) (*sessionEntry, bool) {
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	e, ok := t.sessions[id]
	return e, ok
}

func (t *Transport) resolveSessionWorkspace(ctx context.Context, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	workspaceID, err := runtimetypes.ResolveMessageIndexWorkspace(
		ctx, t.deps.DB.WithoutTransaction(), acpClientIdentity, name)
	if err != nil || workspaceID == "" {
		return "", false
	}
	return workspaceID, true
}

// listSessionsPageSize bounds one session/list page; a var so tests can
// exercise paging without minting hundreds of sessions.
var listSessionsPageSize = 100

// sessionListRow is one session/list candidate before pagination
// (hasTime=false when the session has no messages yet).
type sessionListRow struct {
	internalID string
	name       string
	updatedAt  time.Time
	hasTime    bool
}

// sessionListRowLess is the freshest-first total order session/list returns:
// timed rows sort by time descending, untimed rows sort last, ties fall back
// to internal id. The order must be total or the pagination cursor is ambiguous.
func sessionListRowLess(a, b sessionListRow) bool {
	if a.hasTime != b.hasTime {
		return a.hasTime
	}
	if a.hasTime && !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.internalID > b.internalID
}

// listSessionsCursor encodes a page boundary as "<unixnano>|<internal id>",
// opaque to clients. The full sort key lets listSessionsResume position
// correctly even if the boundary row changed or vanished between pages.
func listSessionsCursor(r sessionListRow) string {
	ts := ""
	if r.hasTime {
		ts = strconv.FormatInt(r.updatedAt.UnixNano(), 10)
	}
	return ts + "|" + r.internalID
}

// listSessionsResume returns the index of the first row sorting strictly
// after the cursor's boundary key. A malformed cursor degrades to comparing
// by internal id alone.
func listSessionsResume(rows []sessionListRow, cursor string) int {
	tsPart, id, found := strings.Cut(cursor, "|")
	boundary := sessionListRow{internalID: id}
	if !found {
		boundary.internalID = cursor
	} else if tsPart != "" {
		if ns, err := strconv.ParseInt(tsPart, 10, 64); err == nil {
			boundary.updatedAt = time.Unix(0, ns)
			boundary.hasTime = true
		}
	}
	for i, r := range rows {
		if sessionListRowLess(boundary, r) {
			return i
		}
	}
	return len(rows)
}

func (t *Transport) ListSessions(ctx context.Context, req libacp.ListSessionsRequest) (libacp.ListSessionsResponse, error) {
	exec := t.deps.DB.WithoutTransaction()

	// The ACP session id is the message-index name; unnamed rows predate ACP
	// naming and aren't listed. Ordering/pagination happen in Go: mi.id is a
	// random UUID and the store's session order is not this surface's order.
	// The cwd filter applies after pagination, so a filtered page may carry
	// fewer items but the cursor still advances.
	indices, err := runtimetypes.NewMessageStore(exec, t.workspaceID()).
		ListMessageSessions(ctx, acpClientIdentity)
	if err != nil {
		return libacp.ListSessionsResponse{}, fmt.Errorf("could not list sessions: %w", err)
	}

	var all []sessionListRow
	for _, s := range indices {
		if s.Name == "" {
			continue
		}
		all = append(all, sessionListRow{
			internalID: s.ID,
			name:       s.Name,
			updatedAt:  s.UpdatedAt,
			hasTime:    !s.UpdatedAt.IsZero(),
		})
	}
	sort.Slice(all, func(i, j int) bool { return sessionListRowLess(all[i], all[j]) })

	start := 0
	if req.Cursor != "" {
		start = listSessionsResume(all, req.Cursor)
	}
	end := min(start+listSessionsPageSize, len(all))

	store := runtimetypes.New(exec)
	var sessions []libacp.SessionInfo
	for _, row := range all[start:end] {
		info := libacp.SessionInfo{
			SessionID: libacp.SessionID(row.name),
			Title:     t.sessionListTitle(ctx, exec, row.internalID, row.name),
			Cwd:       t.sessionCwd(ctx, store, libacp.SessionID(row.name)),
		}
		// `_meta` attribution: which agent runs an external session, which
		// mission a dispatched unit belongs to — without it a unit's session
		// is indistinguishable from the operator's own chats.
		info.Meta = sessionListMeta(
			t.readSessionAgent(ctx, store, libacp.SessionID(row.name)),
			t.readSessionMission(ctx, store, libacp.SessionID(row.name)),
		)
		if row.hasTime {
			info.UpdatedAt = row.updatedAt.UTC().Format(time.RFC3339)
		}
		if req.Cwd != "" && info.Cwd != "" && info.Cwd != req.Cwd {
			continue
		}
		sessions = append(sessions, info)
	}

	resp := libacp.ListSessionsResponse{Sessions: sessions}
	if end < len(all) {
		resp.NextCursor = listSessionsCursor(all[end-1])
	}
	return resp, nil
}

// sessionListTitle resolves a session/list Title in the fixed precedence:
// the /rename override, then the derived heuristic, then fallback (also on
// read failure — session/list must never error out over a title).
func (t *Transport) sessionListTitle(ctx context.Context, exec libdb.Exec, internalSessionID, fallback string) string {
	if title := sessionTitleOverride(ctx, runtimetypes.New(exec), internalSessionID); title != "" {
		return title
	}
	if title := t.firstUserMessageTitle(ctx, exec, internalSessionID); title != "" {
		return title
	}
	return fallback
}

// sessionTitleScanPage is how many messages one firstUserMessageTitle keyset
// page reads. Small on purpose: the answer is almost always in the first page,
// and session/list runs this once per listed session.
const sessionTitleScanPage = 20

// firstUserMessageTitle derives a session title from the first non-empty,
// non-command-shaped user message, whitespace-collapsed and clipped. Returns
// "" when there is no such message or on read failure. Shared by session/list
// and sessionInfoTitle, so both readers agree. Command turns are skipped:
// persistCommandTurn stores "/doctor" as an ordinary user message, but it is
// an instruction, not a title-worthy subject.
//
// Reads by keyset page rather than pulling the thread: a title lives at the
// front of a conversation, so a session/list over N sessions must not load N
// full histories to find N first lines. Each page resumes from the previous
// page's last row, so a message appended mid-scan cannot shift a boundary and
// hide the very message being looked for.
func (t *Transport) firstUserMessageTitle(ctx context.Context, exec libdb.Exec, internalSessionID string) string {
	store := runtimetypes.NewMessageStore(exec, t.workspaceID())
	filter := runtimetypes.MessagePageFilter{Limit: sessionTitleScanPage}
	for {
		page, err := store.ListMessagesPage(ctx, internalSessionID, filter)
		if err != nil {
			return ""
		}
		for _, row := range page {
			var m taskengine.Message
			if err := json.Unmarshal(row.Payload, &m); err != nil {
				return ""
			}
			if m.Role != "user" {
				continue
			}
			text := strings.TrimSpace(m.Content)
			if text == "" || isCommandShapedText(text) {
				continue
			}
			return truncateSessionListTitle(text)
		}
		if len(page) < sessionTitleScanPage {
			return ""
		}
		filter.After = page[len(page)-1].Cursor()
	}
}

// isCommandShapedText reports whether text is shaped like a slash command,
// known or not, reusing the same recognition Prompt's dispatch uses so the
// two can never drift. A path like "/etc/passwd contains x" fails the shape
// test (second slash) and reads as an ordinary title.
func isCommandShapedText(s string) bool {
	if _, _, ok := parseCommand(s); ok {
		return true
	}
	_, ok := unknownCommandName(s)
	return ok
}

// sessionInfoTitle resolves the live Title pushed on session_info_update, in
// the same precedence as sessionListTitle so both readers agree. Empty when
// the session has neither; callers then omit the Title so the notification
// stays a pure freshness ping.
func (t *Transport) sessionInfoTitle(ctx context.Context, internalSessionID string) string {
	if t.deps.DB == nil || internalSessionID == "" {
		return ""
	}
	exec := t.deps.DB.WithoutTransaction()
	if title := sessionTitleOverride(ctx, runtimetypes.New(exec), internalSessionID); title != "" {
		return title
	}
	return t.firstUserMessageTitle(ctx, exec, internalSessionID)
}

// acpSessionTitleKVPrefix namespaces operator session titles, keyed by the
// internal session id: in ACP the name is the session id, so renaming the
// message index would break every stored reference — the title lives beside it.
const acpSessionTitleKVPrefix = "acp:session_title:"

type sessionTitleRecord struct {
	Title string `json:"title"`
}

// setSessionTitleOverride stores (or, on an empty title, clears) the
// operator's own title for a session; clearing hands the label back to the
// derived heuristic.
func setSessionTitleOverride(ctx context.Context, store runtimetypes.Store, internalSessionID, title string) error {
	if internalSessionID == "" {
		return fmt.Errorf("acpsvc: session title: no session")
	}
	key := acpSessionTitleKVPrefix + internalSessionID
	if title == "" {
		if err := store.DeleteKV(ctx, key); err != nil && !errors.Is(err, libdb.ErrNotFound) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(sessionTitleRecord{Title: title})
	if err != nil {
		return err
	}
	return store.SetKV(ctx, key, raw)
}

// sessionTitleOverride reads the operator's own title for a session, or ""
// when never set. A read failure is "" too — never fail a listing over a label.
func sessionTitleOverride(ctx context.Context, store runtimetypes.Store, internalSessionID string) string {
	if internalSessionID == "" {
		return ""
	}
	var rec sessionTitleRecord
	if err := store.GetKV(ctx, acpSessionTitleKVPrefix+internalSessionID, &rec); err != nil {
		return ""
	}
	return truncateSessionListTitle(rec.Title)
}

const acpSessionCwdKVPrefix = "acp:session_cwd:"

type sessionCwdRecord struct {
	Cwd string `json:"cwd"`
}

// persistSessionCwd records the session's cwd durably so session/list can
// report and filter by it across process restarts (the spec requires cwd on
// SessionInfo).
func (t *Transport) persistSessionCwd(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, cwd string) {
	if cwd == "" {
		return
	}
	raw, err := json.Marshal(sessionCwdRecord{Cwd: cwd})
	if err != nil {
		return
	}
	if err := store.SetKV(ctx, acpSessionCwdKVPrefix+string(sid), raw); err != nil {
		reportErr, _, end := t.tracker().Start(ctx, "persist_cwd", "acp_session", "session_id", string(sid))
		reportErr(err)
		end()
	}
}

// sessionCwd resolves a session's cwd: live entry first, then the durable KV
// record. Empty when neither knows (sessions created before cwd persistence).
func (t *Transport) sessionCwd(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID) string {
	t.sessionMu.Lock()
	entry, ok := t.sessions[sid]
	t.sessionMu.Unlock()
	if ok && entry.Cwd != "" {
		return entry.Cwd
	}
	var rec sessionCwdRecord
	if err := store.GetKV(ctx, acpSessionCwdKVPrefix+string(sid), &rec); err != nil {
		return ""
	}
	return rec.Cwd
}
