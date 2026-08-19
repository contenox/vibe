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

const sessionListTitleMaxLen = 60

const ClientIdentity = "acp-client"

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
	// A session tagged with a root outside this instance's set belongs to another
	// workspace on the same machine partition; open it as if it did not exist rather
	// than replaying a foreign transcript and re-tagging it to this root.
	if !t.sessionLoadableInView(t.sessionCwd(ctx, store, req.SessionID)) {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "load session %q: session %q not found", req.SessionID, req.SessionID)
		reportErr(err)
		return libacp.LoadSessionResponse{}, err
	}
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
	t.markExternalIfPersisted(ctx, store, req.SessionID, entry)
	entry.FiredMissions = t.readSessionFiredMission(ctx, store, req.SessionID)
	t.restoreSessionMission(ctx, store, req.SessionID, entry)

	t.clearToolCallState(req.SessionID)
	_, isExternal := entry.driver.(*externalDriver)
	t.replayMessages(ctx, req.SessionID, messages, isExternal)
	t.reattachNativeTurn(ctx, req.SessionID)
	t.reofferParkedAsks(ctx, req.SessionID, contenoxSessionID)
	// The menu goes out only after the session/load result is on the wire.
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

	// A call's outcome lives only in its later result message, so status must be
	// resolved in a pre-pass.
	statuses := replayToolStatuses(messages, external)

	var users, assistantText, toolCalls, toolResults, failedTools int
	// One messageId per historical message: the spec groups replayed chunks by id.
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

	t.sendUsageUpdate(ctx, sessionID, estimateHistoryTokens(messages))
}

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

func replayStatusFor(statuses map[string]libacp.ToolCallStatus, toolCallID string) libacp.ToolCallStatus {
	if status, ok := statuses[toolCallID]; ok && status != "" {
		return status
	}
	return libacp.ToolCallStatusCompleted
}

// toolExecFailedRE matches the sentence taskengine substitutes for a failed tool
// result; it couples to the engine's error formatting.
var toolExecFailedRE = regexp.MustCompile(`^tool [^\s]+ execution failed: `)

// replayToolStatus derives a native tool call's terminal status from its persisted
// result. The {"error": ...} test requires exactly that one key, so a result
// carrying an error field beside others replays as completed.
func replayToolStatus(content string) libacp.ToolCallStatus {
	s := strings.TrimSpace(content)
	if s == "" {
		return libacp.ToolCallStatusCompleted
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal([]byte(s), &unquoted); err == nil {
			s = strings.TrimSpace(unquoted)
		}
	}
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
// usage_update, using the same arithmetic as the engine's tokenizer. It
// under-counts and snaps to exact on the next token_usage event.
func estimateHistoryTokens(messages []taskengine.Message) int {
	total := 0
	for _, m := range messages {
		total += estimateContentTokens(m.Content)
	}
	return total
}

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

func errSetupRequired() error {
	return libacp.NewError(libacp.ErrAuthRequired, "contenox is not configured yet: no default-model is set. Run the \"Setup Contenox\" auth method, set the CONTENOX_DEFAULT_* environment variables (or run `contenox acp --setup`), then reconnect.")
}

// requireSessionCwd rejects a malformed session cwd before any side effects. The
// "/" sentinel deliberately survives: it means the machine's default root.
func requireSessionCwd(cwd string) error {
	if strings.TrimSpace(cwd) == "" {
		return libacp.NewError(libacp.ErrInvalidParams, "cwd is required")
	}
	if cwd == "/" {
		// The sentinel survives this precheck; resolveWorkspaceCwd settles it
		// against the host root, or refuses it where none is configured.
		return nil
	}
	if _, err := vfs.ResolveSessionCwd(nil, cwd, ""); err != nil {
		return libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	return nil
}

// resolveWorkspaceCwd maps a requested session cwd onto the concrete root to
// use. With a host root configured (serve), the cwd must resolve under it;
// without one (an editor over ACP), the client's cwd is authoritative and only
// control-plane paths are refused — the client owns the workspace.
func (t *Transport) resolveWorkspaceCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(t.deps.WorkspaceRoots, cwd, "")
	if err != nil {
		return "", libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	return resolved, nil
}

// resolveExistingSessionCwd is resolveWorkspaceCwd plus one rule: the sentinel "/"
// sent on every load/resume must not clobber the session's stored workspace.
func (t *Transport) resolveExistingSessionCwd(ctx context.Context, store runtimetypes.Store, sid libacp.SessionID, cwd string) (string, error) {
	if cwd == "" || cwd == "/" {
		if existing := t.sessionCwd(ctx, store, sid); existing != "" {
			if f := t.deps.WorkspaceRoots; f != nil {
				if resolved, ok := f.Allows(existing); ok {
					return resolved, nil
				}
			} else {
				// No host root: the stored workspace was validated when the
				// session was created, and nothing narrower exists to check
				// it against now.
				return existing, nil
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

	// contenox.adopt takes precedence over contenox.agent: nothing is spawned.
	if ref, ok := parseAdoptMeta(req.Meta); ok {
		resp, adoptErr := t.newAdoptedSession(ctx, internalID, sessionID, sessionCwd, workspaceID, store, ref, reportChange)
		if adoptErr != nil {
			reportErr(adoptErr)
			return libacp.NewSessionResponse{}, adoptErr
		}
		return resp, nil
	}

	if agentName := parseAgentMeta(req.Meta); agentName != "" {
		// Bring the downstream agent up first, so a spawn failure fails
		// session/new with no session and no leaked process.
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
		t.persistSessionInstance(ctx, sessionID, att.instanceID)
		t.persistSessionDownstream(ctx, sessionID, att.downstreamID)
		t.clearToolCallState(sessionID)

		// The menu was cached before the client could resolve the session id.
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
	// allowlists in `_meta`.
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
		HITLPolicy:        missionHITLPolicy(missionMeta.HITLPolicyName),
	}
	t.sessionMu.Lock()
	t.sessions[sessionID] = entry
	t.bindContenoxSession(contenoxSessionID, sessionID)
	t.sessionMu.Unlock()
	t.persistSessionCwd(ctx, store, sessionID, sessionCwd)
	t.persistSessionMission(ctx, store, sessionID, missionMeta)
	t.clearToolCallState(sessionID)

	// An available_commands_update sent before the session/new result is dropped.
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

// ResumeSession is session/load without the history replay: the client kept its
// transcript and only needs the server-side session re-bound.
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
	// See LoadSession: a session rooted outside this instance's set is foreign and
	// is refused as if unknown, never re-bound and re-tagged to this root.
	if !t.sessionLoadableInView(t.sessionCwd(ctx, store, req.SessionID)) {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "resume session %q: session %q not found", req.SessionID, req.SessionID)
		reportErr(err)
		return libacp.ResumeSessionResponse{}, err
	}
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
	entry.FiredMissions = t.readSessionFiredMission(ctx, store, req.SessionID)
	t.restoreSessionMission(ctx, store, req.SessionID, entry)
	t.clearToolCallState(req.SessionID)
	t.reattachNativeTurn(ctx, req.SessionID)
	t.reofferParkedAsks(ctx, req.SessionID, contenoxSessionID)

	if _, isExternal := entry.driver.(*externalDriver); isExternal {
		t.reemitExternalCommandMenu(ctx, store, req.SessionID)
	} else if entry.driver.AvailableCommands() != nil {
		libacp.AfterResponse(ctx, func() {
			t.sendAvailableCommands(ctx, req.SessionID)
		})
	}

	// Resume keeps the client's transcript but not its gauge, and nothing may
	// precede its response.
	libacp.AfterResponse(ctx, func() {
		t.sendResumedUsageUpdate(ctx, req.SessionID, entry)
	})

	reportChange(string(req.SessionID), map[string]any{
		"contenox_session_id": contenoxSessionID,
	})
	return libacp.ResumeSessionResponse{ConfigOptions: t.reloadedConfigOptions(ctx, store, req.SessionID, entry)}, nil
}

// SetSessionMode is not supported: the equivalent controls are session config
// options, and initialize never returns a Modes state.
func (t *Transport) SetSessionMode(_ context.Context, _ libacp.SetSessionModeRequest) (libacp.SetSessionModeResponse, error) {
	return libacp.SetSessionModeResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetMode)
}

// SetSessionModel is not supported: an external session's model picker is
// surfaced as the AgentModelConfigOptionID config option instead.
func (t *Transport) SetSessionModel(_ context.Context, _ libacp.SetSessionModelRequest) (libacp.SetSessionModelResponse, error) {
	return libacp.SetSessionModelResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetModel)
}

// CloseSession releases a session's connection-local resources without touching
// its stored history; closing an unknown session succeeds. Close detaches, delete
// stops: the kernel's per-session state is left behind so a reconnect can
// re-attach with the downstream agent's context.
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
	if entry != nil {
		_ = entry.driver.Close()
	}
	t.clearToolCallState(req.SessionID)
	// Unlike a bare connection drop, an explicit close tears the shell down.
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
	// Unlike close, a delete must terminate the instance outright.
	if entry != nil {
		_ = entry.driver.Close()
	}
	if t.deps.Instances != nil {
		if instanceID := t.readSessionInstance(ctx, store, req.SessionID); instanceID != "" {
			_ = t.deps.Instances.Stop(instanceID)
		}
	}
	if t.deps.NativeTurns != nil {
		t.deps.NativeTurns.Cancel(req.SessionID)
	}
	t.clearToolCallState(req.SessionID)

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
	_ = store.DeleteKV(ctx, acpSessionAgentCommandsKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionAgentConfigOptionsKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionHITLPolicyKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionInstanceKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionDownstreamKVPrefix+string(req.SessionID))
	_ = store.DeleteKV(ctx, acpSessionMissionKVPrefix+string(req.SessionID))

	reportChange(string(req.SessionID), map[string]any{"was_open": entry != nil})
	return libacp.DeleteSessionResponse{}, nil
}

func (t *Transport) dropSessionEntry(sid libacp.SessionID) *sessionEntry {
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
// left behind by a previous process.
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

	// A bare connection drop stops streaming but does not kill shells.

	t.sessionMu.Lock()
	entries := make([]*sessionEntry, 0, len(t.sessions))
	sids := make([]libacp.SessionID, 0, len(t.sessions))
	for sid, e := range t.sessions {
		entries = append(entries, e)
		sids = append(sids, sid)
		t.deps.SessionRouter.unbind(e.InternalSessionID, t)
	}
	t.sessions = make(map[libacp.SessionID]*sessionEntry)
	t.contenoxToACPID = make(map[string]libacp.SessionID)
	t.sessionMu.Unlock()

	for _, sid := range sids {
		t.cancelInflightPrompt(sid)
	}

	for _, e := range entries {
		t.cleanupMcpServers(ctx, store, e.McpServerNames)
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
		ctx, t.deps.DB.WithoutTransaction(), ClientIdentity, name)
	if err != nil || workspaceID == "" {
		return "", false
	}
	return workspaceID, true
}

var listSessionsPageSize = 100

type sessionListRow struct {
	internalID string
	name       string
	updatedAt  time.Time
	hasTime    bool
}

// sessionListRowLess is the freshest-first total order session/list returns. It
// must be total, or the pagination cursor is ambiguous.
func sessionListRowLess(a, b sessionListRow) bool {
	if a.hasTime != b.hasTime {
		return a.hasTime
	}
	if a.hasTime && !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.internalID > b.internalID
}

func listSessionsCursor(r sessionListRow) string {
	ts := ""
	if r.hasTime {
		ts = strconv.FormatInt(r.updatedAt.UnixNano(), 10)
	}
	return ts + "|" + r.internalID
}

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

	// The ACP session id is the message-index name; unnamed rows predate ACP naming
	// and aren't listed. Ordering and pagination happen in Go. Both scopes are
	// applied here, BEFORE pagination, so pages stay full and the cursor stays
	// correct; the stored root is a per-session KV, so this costs one read per
	// candidate.
	indices, err := runtimetypes.NewMessageStore(exec, t.workspaceID()).
		ListMessageSessions(ctx, ClientIdentity)
	if err != nil {
		return libacp.ListSessionsResponse{}, fmt.Errorf("could not list sessions: %w", err)
	}

	store := runtimetypes.New(exec)
	var all []sessionListRow
	for _, s := range indices {
		if s.Name == "" {
			continue
		}
		storedRoot := t.sessionCwd(ctx, store, libacp.SessionID(s.Name))
		if !t.sessionInWorkspaceView(storedRoot) {
			continue
		}
		if !sessionUnderRequestedCwd(storedRoot, req.Cwd) {
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

	var sessions []libacp.SessionInfo
	for _, row := range all[start:end] {
		info := libacp.SessionInfo{
			SessionID: libacp.SessionID(row.name),
			Title:     t.sessionListTitle(ctx, exec, row.internalID, row.name),
			Cwd:       t.sessionCwd(ctx, store, libacp.SessionID(row.name)),
		}
		// `_meta` attribution: which agent runs an external session, and which
		// mission a dispatched unit belongs to.
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

func (t *Transport) sessionListTitle(ctx context.Context, exec libdb.Exec, internalSessionID, fallback string) string {
	if title := sessionTitleOverride(ctx, runtimetypes.New(exec), internalSessionID); title != "" {
		return title
	}
	if title := t.firstUserMessageTitle(ctx, exec, internalSessionID); title != "" {
		return title
	}
	return fallback
}

const sessionTitleScanPage = 20

// firstUserMessageTitle derives a session title from the first non-empty,
// non-command-shaped user message. It reads by keyset page so a session/list over
// N sessions does not load N full histories.
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

// isCommandShapedText reports whether text is shaped like a slash command, reusing
// the recognition Prompt's dispatch uses so the two cannot drift.
func isCommandShapedText(s string) bool {
	if _, _, ok := parseCommand(s); ok {
		return true
	}
	_, ok := unknownCommandName(s)
	return ok
}

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

const acpSessionTitleKVPrefix = "acp:session_title:"

type sessionTitleRecord struct {
	Title string `json:"title"`
}

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

// sessionInWorkspaceView is the listing scope: the editor shape (nil factory) has
// no server root, so everything is in view and req.Cwd stays the only filter;
// otherwise the stored root must be in this instance's root set.
func (t *Transport) sessionInWorkspaceView(storedRoot string) bool {
	f := t.deps.WorkspaceRoots
	if f == nil {
		return true
	}
	return f.InView(storedRoot)
}

// sessionLoadableInView is the load/resume scope: it differs from the listing
// scope only in admitting a legacy/untagged session (stored root ""), which the
// loading instance then claims and re-tags — listing excludes it, opening adopts it.
func (t *Transport) sessionLoadableInView(storedRoot string) bool {
	if storedRoot == "" {
		return true
	}
	return t.sessionInWorkspaceView(storedRoot)
}

// sessionUnderRequestedCwd narrows a listing to one workspace. An empty or "/"
// request asks for every session this instance can see; anything else lists
// only sessions rooted at that directory or below it. Without this a client
// that serves no host root — beam, an editor — resumes the newest session on
// the machine rather than the newest one here.
func sessionUnderRequestedCwd(storedRoot, requested string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == "/" {
		return true
	}
	if storedRoot == "" {
		return false
	}
	resolved, err := vfs.Contain(requested, storedRoot)
	return err == nil && resolved != ""
}
