package acpsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/chatservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/vfs"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
)

// sessionListTitleMaxLen bounds SessionInfo.Title derived from a session's
// first user message, so a humane session picker never has to render a
// multi-paragraph prompt as its label.
const sessionListTitleMaxLen = 60

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
	if !filepath.IsAbs(req.Cwd) {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "cwd must be an absolute path, got %q", req.Cwd)
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
	// Re-flag an external session from its persisted agent name so its config
	// options come back minimal and the next prompt routes to the downstream
	// agent (lazily respawned). The transcript is replayed below either way; the
	// downstream process is deliberately NOT resurrected during load.
	t.markExternalIfPersisted(ctx, store, req.SessionID, entry)
	// A reloaded session that fired missions is still their supervisor: restore the
	// flag so its next turn is offered the supervisor tools it had before the
	// reload. The missions certainly outlived the connection.
	entry.FiredMissions = t.readSessionFiredMission(ctx, store, req.SessionID)

	t.clearToolCallState(req.SessionID)
	t.subscribeTerminal(req.SessionID, contenoxSessionID)
	// markExternalIfPersisted (above) has already swapped the driver, so the flag
	// reflects how this session's tool messages were persisted: an external
	// session stored each tool call as a self-contained ACP record, a native one
	// stored assistant CallTools + tool-result text.
	_, isExternal := entry.driver.(*externalDriver)
	t.replayMessages(ctx, req.SessionID, messages, isExternal)
	// Reconnect: if a native turn is still in flight for this session (a browser
	// reloaded mid-turn), join it as a viewer so the client resumes the live stream
	// from the turn journal. Ordered after the transcript replay above, which the
	// journal never overlaps (persisted turns vs. the one still running).
	t.reattachNativeTurn(ctx, req.SessionID)
	// Emit the slash-command menu only after the session/load result is on the wire
	// (see sendAvailableCommands) so the client can resolve the session. A native
	// session emits its contenox menu (and banner). An external session has no native
	// menu (AvailableCommands is nil); its downstream agent's menu — dead with the
	// pre-load connection and not resurrected until the next prompt — is re-emitted
	// from the values persisted at session/new, so the reopened session shows it
	// without a first prompt.
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

	// The persisted transcript records a tool call's OUTCOME nowhere but in the
	// text of its result message, and the assistant message that OPENED the call
	// is stored BEFORE that result. So the terminal status has to be resolved in
	// a pre-pass over the whole transcript, not discovered as the loop walks it —
	// otherwise every replayed tool_call is emitted as "completed" and a call
	// that actually failed comes back after a relaunch wearing a green check.
	// See replayToolStatuses for how honest that mapping can be.
	statuses := replayToolStatuses(messages, external)

	var users, assistantText, toolCalls, toolResults, failedTools int
	// One messageId per historical message: the spec groups replayed chunks by
	// id, so thinking + text of one assistant turn render as one message and a
	// change of id marks the next.
	for i, m := range messages {
		messageID := fmt.Sprintf("replay-%d", i)
		switch m.Role {
		case "user":
			if m.Content == "" {
				continue
			}
			update := libacp.NewUserMessageChunk(m.Content)
			update.MessageID = messageID
			t.sendUpdate(ctx, libacp.SessionNotification{
				SessionID: sessionID,
				Update:    update,
			})
			users++
		case "assistant":
			if m.Thinking != "" {
				update := libacp.NewAgentThoughtChunk(m.Thinking)
				update.MessageID = messageID
				t.sendUpdate(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    update,
				})
			}
			if m.Content != "" {
				update := libacp.NewAgentMessageChunk(m.Content)
				update.MessageID = messageID
				t.sendUpdate(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    update,
				})
				assistantText++
			}
			for _, tc := range m.CallTools {
				status := replayStatusFor(statuses, tc.ID)
				t.sendUpdate(ctx, libacp.SessionNotification{
					SessionID: sessionID,
					Update:    toolCallUpdateFromCall(tc, status),
				})
				toolCalls++
				if status == libacp.ToolCallStatusFailed {
					failedTools++
				}
			}
		case "tool":
			// An external session's tool message carries a self-contained ACP tool
			// record (title/kind/input/output/diffs the downstream produced); replay
			// it as one complete tool_call update. A native session's tool message is
			// raw result text paired with the assistant's CallTools above.
			update := toolCallUpdateFromResult(m)
			if external {
				if u, ok := externalToolReplayUpdate(m); ok {
					update = u
				}
			}
			t.sendUpdate(ctx, libacp.SessionNotification{
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

	// A loaded session used to get the SIZE-only usage_update a brand-new session
	// gets, which reads on the wire as "0 of N used" — a gauge that lies about a
	// full history until the next turn happens to correct it. Carry the used half
	// too. See estimateHistoryTokens for exactly how well-founded that number is.
	t.sendUsageUpdate(ctx, sessionID, estimateHistoryTokens(messages))
}

// replayToolStatuses maps each replayed tool call id to the TERMINAL status its
// persisted result records, so the assistant message that opened the call can be
// replayed with the outcome that call actually had.
//
// An EXTERNAL session is exact: persistExternalTurn stored the downstream
// agent's own ACP tool record, status field and all, so this reads it back
// verbatim (the same record externalToolReplayUpdate decodes).
//
// A NATIVE session is a derivation, and the fragility is worth naming: the
// store holds no status column for a tool call at all — a tool message is a
// role, a tool_call_id and the result TEXT (see taskengine's tool result
// message), and the live tool_call event that carried Error is long gone by
// the time a session is reloaded. So the outcome is recovered from the two
// shapes the engine itself writes for a failure, and nothing else:
//
//   - the string "tool <name> execution failed: <err>", which taskexec
//     substitutes for the result when a tool returns an error; and
//   - the object {"error": "<err>"}, which toolErrorContent writes for a call
//     that was interrupted before it produced one.
//
// That coupling is to the engine's own error-formatting, so it breaks silently
// if that formatting changes — a replayed card would go back to claiming
// success. The alternative (persisting the status) is a store change; until
// then this is the honest ceiling, and it is deliberately narrow: see
// replayToolStatus for why a result that merely CONTAINS an error field is not
// treated as a failure.
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
// no result message in the transcript — the unanswered tail of a suspended
// batch, or a transcript truncated mid-turn — keeps the historical "completed",
// because "we never recorded an outcome" is not evidence of failure and a red
// cross would be a louder lie than the green check.
func replayStatusFor(statuses map[string]libacp.ToolCallStatus, toolCallID string) libacp.ToolCallStatus {
	if status, ok := statuses[toolCallID]; ok && status != "" {
		return status
	}
	return libacp.ToolCallStatusCompleted
}

// toolExecFailedRE matches the exact sentence taskengine substitutes for a tool
// result when the tool returned an error ("tool <name> execution failed: ...").
// Anchored and shaped to a single unspaced tool name so ordinary tool OUTPUT
// that happens to discuss a failure is not mistaken for one.
var toolExecFailedRE = regexp.MustCompile(`^tool [^\s]+ execution failed: `)

// replayToolStatus derives one native tool call's terminal status from its
// persisted result content. See replayToolStatuses for the whole picture.
//
// The {"error": ...} test requires the object to hold that ONE key, which is
// what toolErrorContent writes and nothing else does. A result that merely
// CARRIES an error field alongside other fields is a SUCCESSFUL call reporting
// what it found — local_shell's response object has an `error` beside
// `exit_code`/`stdout` for a command that exited non-zero, and the live path
// shows that as a completed call (the tool ran; the command failed). Replay
// must agree with the live path, not invent a second verdict.
func replayToolStatus(content string) libacp.ToolCallStatus {
	s := strings.TrimSpace(content)
	if s == "" {
		return libacp.ToolCallStatusCompleted
	}
	// A tool result is serialized through json.Marshal for the DataTypeAny the
	// engine reports on every error path, so the failure sentence arrives as a
	// JSON string literal. Unwrap it before matching.
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal([]byte(s), &unquoted); err == nil {
			s = strings.TrimSpace(unquoted)
		}
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

// estimateHistoryTokens is the "used" half of the usage_update a reloaded
// session is given, derived from the transcript that was just replayed.
//
// It is an ESTIMATE, and the reason is worth stating plainly rather than
// hiding behind a number: nothing durable records what a past turn actually
// consumed. The engine's token_usage event is a live bus event and is not
// journaled on this deployment; the captured execution state persists a
// TokenUsage whose prompt half is never populated; and the message store has no
// usage column. What IS knowable is the input the next turn will re-send — the
// history itself — so this recomputes it the same way the engine will:
// ollamatokenizer.EstimateTokenizer (the tokenizer enginesvc wires
// unconditionally) counts runes/4 with a floor of 1 per non-empty string, over
// each message's Content, which is exactly the loop taskexec runs to produce
// the number the gauge shows during a turn.
//
// It therefore UNDER-counts by the two components only the turn itself knows:
// the chain's system prelude and the JSON of the resolved tool schemas. The
// gauge comes back close instead of at zero, and snaps to exact on the first
// token_usage event of the next turn. Deliberately not corrected by a guessed
// constant — a wrong number stated confidently is worse than a low one.
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
// message that opened it. status is the outcome resolved from the transcript's
// LATER tool-result message (see replayToolStatuses) — the call site owns it,
// because this message alone cannot know how the call ended.
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

// externalToolReplayUpdate decodes an external session's persisted tool message
// (an externalToolRecord JSON written by persistExternalTurn) into the single
// tool_call update that reconstructs its card — the downstream agent's own
// title, kind, input, output, diffs, and locations, verbatim. Returns false when
// the content is not an external record (so the caller falls back to the native
// result mapping), keeping native replay untouched.
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
		// The spec requires a title on tool_call notifications; fall back to the id
		// so strict clients still register the card.
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
// tool call. Its status is derived from the result content itself — the same
// derivation replayToolStatuses applies to the opening card, so the two halves
// of one replayed call always agree.
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

// errSetupRequired is returned by session operations when the transport is
// running setup-only (no default-model was configured at launch, so the engine
// is nil). It gives the ACP client an actionable message instead of letting the
// nil engine panic on first use. initialize/authenticate stay available so the
// "Setup Contenox" terminal auth method (or the env_var method) can configure a
// model. The code is the spec's -32000 auth_required: a conformant client
// reacts by offering the advertised auth methods, which is exactly the setup
// flow.
func errSetupRequired() error {
	return libacp.NewError(libacp.ErrAuthRequired, "contenox is not configured yet: no default-model is set. Run the \"Setup Contenox\" auth method, set the CONTENOX_DEFAULT_* environment variables (or run `contenox acp --setup`), then reconnect.")
}

// resolveWorkspaceCwd maps a requested session cwd onto the concrete root the
// session will use, delegating the DECISION to vfs.ResolveSessionCwd — the one
// implementation shared with fleetservice's dispatch path — and owning only the
// translation of its refusal into this transport's wire error. The fallback is
// "": the ACP transport has no project root of its own, so an unspecified cwd on
// the stdio path (no allowlist configured) stays unspecified and the editor's own
// working directory governs. See vfs.ResolveSessionCwd for the full rule set.
func (t *Transport) resolveWorkspaceCwd(cwd string) (string, error) {
	resolved, err := vfs.ResolveSessionCwd(t.deps.WorkspaceRoots, cwd, "")
	if err != nil {
		return "", libacp.NewError(libacp.ErrInvalidParams, err.Error())
	}
	return resolved, nil
}

// resolveExistingSessionCwd resolves the cwd for session/load and session/resume
// on an existing session. It is resolveWorkspaceCwd plus exactly ONE extra rule,
// which is a transport policy and not a filesystem judgement: the sentinel "/" or
// empty cwd — what beam sends on every load/resume — must NOT clobber the
// session's stored workspace back to the default, so the persisted cwd wins when
// it is still allowlisted. Everything else (the absolute-path guard, the
// allowlist check, the default for an unspecified cwd, the no-allowlist
// passthrough) is the shared procedure, reached through resolveWorkspaceCwd.
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

	if !filepath.IsAbs(req.Cwd) {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "cwd must be an absolute path, got %q", req.Cwd)
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

	// A client ADOPTS an ALREADY-RUNNING instance+session (typically one a fleet
	// dispatch created and left unwatched) via the contenox.adopt `_meta` key. It sits
	// beside contenox.agent in this one routing switch and takes precedence over it:
	// adopt names a concrete live instance, so an accompanying agent name would be at
	// best redundant and at worst a mislabel (attribution comes from the instance — see
	// newAdoptedSession). Nothing is spawned. Absent/malformed, this falls through to the
	// two paths below unchanged.
	if ref, ok := parseAdoptMeta(req.Meta); ok {
		resp, adoptErr := t.newAdoptedSession(ctx, internalID, sessionID, sessionCwd, workspaceID, store, ref, reportChange)
		if adoptErr != nil {
			reportErr(adoptErr)
			return libacp.NewSessionResponse{}, adoptErr
		}
		return resp, nil
	}

	// A client binds this session to a REGISTERED external ACP agent via the
	// contenox.agent `_meta` key; absent, the native chain path below runs
	// unchanged (byte-for-byte the historical behavior).
	if agentName := parseAgentMeta(req.Meta); agentName != "" {
		// Bring up and drive the downstream agent first: an unknown/disabled agent or a
		// spawn/handshake failure must fail session/new cleanly with NO session and
		// NO leaked process/instance. The upstream client's req.McpServers are for the
		// chain engine (unused here); the downstream agent gets its own declared
		// allowlist. When Deps.Instances is wired the downstream is a Manager-owned
		// instance that survives this connection; otherwise a connCtx-bound subprocess.
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
			wrapped := fmt.Errorf("acpsvc: agent.SessionNew: %w", sessErr)
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
		// Persist the Manager instance id AND the downstream session id (both no-ops on
		// the nil-Instances path) so a later session/load re-attaches to THIS still-running
		// instance and drives its SAME downstream session, preserving the agent's context.
		t.persistSessionInstance(ctx, sessionID, att.instanceID)
		t.persistSessionDownstream(ctx, sessionID, att.downstreamID)
		t.clearToolCallState(sessionID)

		// The downstream agent advertises its slash-command menu immediately after
		// its own session/new (an available_commands_update the bridge cached without
		// relaying — a menu delivered before THIS session/new response references a
		// session id the upstream client has not learned and is dropped). Re-emit the
		// cached menu once the result is on the wire, mirroring the native menu's
		// sendAvailableCommands scheduling (see externalBridge.markBound).
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
		wrapped := fmt.Errorf("acpsvc: agent.SessionNew: %w", err)
		reportErr(wrapped)
		return libacp.NewSessionResponse{}, wrapped
	}

	// A dispatched unit's session/new carries its mission id in `_meta`
	// (missionservice.MissionMetaKey). It has neither contenox.agent nor
	// contenox.adopt, so it falls through to THIS native path — the unit runs its
	// own contenox chain, which is where its mission tools live. Binding the id
	// onto the entry here (construction) is what scopes those tools to this one
	// mission; an ordinary chat session has no such `_meta` and reads as "".
	missionID, _ := missionservice.ParseMissionMeta(req.Meta)

	entry := &sessionEntry{
		WorkspaceID:       workspaceID,
		Cwd:               sessionCwd,
		InternalSessionID: contenoxSessionID,
		McpServerNames:    registered,
		MissionID:         missionID,
		driver:            &nativeDriver{t: t, agent: ag},
		Provider:          t.provider(),
		Model:             t.model(),
		Think:             t.thinkDefault(),
		HITLPolicy:        hitlPolicyDefaultValue,
	}
	t.sessionMu.Lock()
	t.sessions[sessionID] = entry
	t.bindContenoxSession(contenoxSessionID, sessionID)
	t.sessionMu.Unlock()
	t.persistSessionCwd(ctx, store, sessionID, sessionCwd)
	t.persistSessionMission(ctx, store, sessionID, missionID)
	t.clearToolCallState(sessionID)
	t.subscribeTerminal(sessionID, contenoxSessionID)

	// A client learns this new session's id only from the session/new result;
	// emitting available_commands_update before that result makes the client drop
	// it as an unknown session (and the slash-command menu never appears). Defer
	// it until libacp has written the result.
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
	if !filepath.IsAbs(req.Cwd) {
		err := libacp.NewErrorf(libacp.ErrInvalidParams, "cwd must be an absolute path, got %q", req.Cwd)
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
	t.clearToolCallState(req.SessionID)
	t.subscribeTerminal(req.SessionID, contenoxSessionID)
	// Reconnect (transcript kept client-side): join an in-flight native turn so the
	// resumed session picks the live stream back up. Same no-overlap reasoning as
	// LoadSession; a no-op when no turn is in flight.
	t.reattachNativeTurn(ctx, req.SessionID)

	// Mirror LoadSession: a native session re-advertises its contenox menu; an
	// external session re-emits its downstream agent's persisted menu (the live bridge
	// died with the pre-resume connection and is not respawned until the next prompt).
	if _, isExternal := entry.driver.(*externalDriver); isExternal {
		t.reemitExternalCommandMenu(ctx, store, req.SessionID)
	} else if entry.driver.AvailableCommands() != nil {
		libacp.AfterResponse(ctx, func() {
			t.sendAvailableCommands(ctx, req.SessionID)
		})
	}

	// A resume keeps the client's TRANSCRIPT, not its gauge: the usage_update is a
	// wire fact the reconnecting client no longer holds, and without one the
	// indicator sat at zero over a full history until the next turn happened to
	// move it. Resume replays nothing, so the history is read rather than handed
	// over — and it is pushed AFTER the result, next to the menu, because resume's
	// contract is that nothing precedes its response (that is what distinguishes
	// it from load).
	libacp.AfterResponse(ctx, func() {
		t.sendResumedUsageUpdate(ctx, req.SessionID, entry)
	})

	reportChange(string(req.SessionID), map[string]any{
		"contenox_session_id": contenoxSessionID,
	})
	return libacp.ResumeSessionResponse{ConfigOptions: t.reloadedConfigOptions(ctx, store, req.SessionID, entry)}, nil
}

// SetSessionMode is not supported: contenox does not model the Ask/Code
// session mode toggle some ACP editors send as a first-class session/set_mode
// capability —
// the equivalent controls (model, HITL policy, think level) are exposed as
// session config options instead. Initialize never returns a Modes state in
// session/new or session/load, so a conformant client will never call this.
func (t *Transport) SetSessionMode(_ context.Context, _ libacp.SetSessionModeRequest) (libacp.SetSessionModeResponse, error) {
	return libacp.SetSessionModeResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetMode)
}

// SetSessionModel is not supported on contenox's OWN upstream surface: the runtime
// never advertises a `models` state (SessionModelState) to its clients — for an
// external session the DOWNSTREAM agent's model picker is surfaced as the synthetic
// AgentModelConfigOptionID config option and switched via set_config_option (which the
// driver translates to the downstream's session/set_model), and a native session
// exposes no model picker of this UNSTABLE shape at all. So a conformant client never
// calls this; it reports MethodNotFound, mirroring SetSessionMode.
func (t *Transport) SetSessionModel(_ context.Context, _ libacp.SetSessionModelRequest) (libacp.SetSessionModelResponse, error) {
	return libacp.SetSessionModelResponse{}, libacp.MethodNotFound(libacp.MethodSessionSetModel)
}

// CloseSession releases the connection-local resources of a session without
// touching its stored history. Closing an unknown session succeeds: the
// desired state (not open here) already holds.
//
// It deliberately does NOT call agentinstance.Manager.CloseSession, and the
// kernel's per-session state (its captured surface, its journal) is deliberately
// LEFT BEHIND. That is not an oversight and not a leak:
//
//   - close DETACHES, delete STOPS. An instance outlives a closed session on
//     purpose so a reconnect can re-attach and keep the downstream agent's
//     context (externalDriver.ensureAttached); the retained state is what that
//     path and session/load read. Dropping it would leave the subprocess running
//     while the kernel forgot the session it is still driving.
//   - InstanceStatus.SessionIDs is sourced from that state, and it is what
//     fleetservice.Cancel fans out over and what resolveAdoptTarget validates
//     against. A supervisor closing their tab on an ADOPTED dispatch must not
//     make the running unit's session un-cancellable and un-adoptable — see
//     adopt.go's "Honest limitations".
//
// The retention is bounded: at most one open session per instance in every path
// that exists today, a journal that is a fixed-size ring, and a viewer registry
// this close empties via the driver's detach. All of it is reclaimed when the
// instance stops — which is exactly what DeleteSession does.
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
	// An explicit close ends the session on this connection: tear down its driver
	// (an external driver closes its downstream agent now, rather than waiting for
	// connection teardown to reap it; the native driver is a no-op).
	if entry != nil {
		_ = entry.driver.Close()
	}
	t.clearToolCallState(req.SessionID)
	// An explicit close is a user action ending the session on this connection —
	// tear down its shell (unlike a bare connection drop, which keeps the shell
	// alive for reconnect and lets the idle reaper reclaim it).
	t.closeTerminal(req.SessionID, entry)
	reportChange(string(req.SessionID), map[string]any{"was_open": entry != nil})
	return libacp.CloseSessionResponse{}, nil
}

// DeleteSession removes the session's stored history (and any connection-local
// state). Per spec, deleting a nonexistent session succeeds silently, and the
// session disappears from session/list.
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
	// The session is being deleted; its driver's downstream agent (if any) must
	// not outlive it. Close detaches a Manager-owned instance (leaving it Running);
	// the explicit stop below is what actually ends it — a delete, unlike a plain
	// close/disconnect, terminates the instance.
	if entry != nil {
		_ = entry.driver.Close()
	}
	// Stop the Manager-owned instance backing this external session, if any. The
	// persisted instanceID is the durable source of truth (the session may not be
	// open on this connection, so entry can be nil). A no-op on the nil-Instances
	// path and for native sessions.
	if t.deps.Instances != nil {
		if instanceID := t.readSessionInstance(ctx, store, req.SessionID); instanceID != "" {
			_ = t.deps.Instances.Stop(instanceID)
		}
	}
	// A delete destroys the session, so its in-flight NATIVE turn (running on the
	// survival Registry, off any connection) must not outlive it. Unlike a plain
	// close/disconnect — which keeps the turn alive for reconnect — a delete
	// terminates it. A no-op on the nil-Registry path, for external sessions, and
	// when no turn is in flight.
	if t.deps.NativeTurns != nil {
		t.deps.NativeTurns.Cancel(req.SessionID)
	}
	t.clearToolCallState(req.SessionID)
	// The session's history is being deleted; its shell must not outlive it.
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
	// The downstream-surface keys (command menu, config options), the external
	// session's per-session HITL policy, and its Manager instance + downstream session
	// ids are meaningful only alongside the agent-name key; drop them with it.
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
	// Abort any in-flight turn before the session's connection-local state goes
	// away: a Close/Delete that races a running prompt must stop the chain, not
	// let it keep executing against a session that no longer exists here. A clean
	// no-op when nothing is running.
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
			return nil, fmt.Errorf("acpsvc: invalid mcp server %q: %w", srv.Name, err)
		}
		name := mcpNameFor(t.mcpOwnerID(), sessionID, srv.Name)
		row := mcpRowFromLibacp(name, srv)
		if err := store.UpsertMCPServerByName(ctx, row); err != nil {
			t.cleanupMcpServers(ctx, store, registered)
			return nil, fmt.Errorf("acpsvc: register mcp server %q: %w", srv.Name, err)
		}
		if t.deps.Engine != nil && t.deps.Engine.MCPManager != nil {
			if err := t.deps.Engine.MCPManager.StartWorker(ctx, row); err != nil {
				registered = append(registered, name)
				t.cleanupMcpServers(ctx, store, registered)
				return nil, fmt.Errorf("acpsvc: start mcp worker %q: %w", srv.Name, err)
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
			return nil, fmt.Errorf("acpsvc: list mcp servers for runtime allowlist: %w", err)
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

	// A bare connection drop stops streaming but does NOT kill shells: a browser
	// reload reconnects and re-subscribes, and persistent shells are the point.
	// Idle-timeout reclaims anything genuinely abandoned.
	t.unsubscribeAllTerminals()

	t.sessionMu.Lock()
	entries := make([]*sessionEntry, 0, len(t.sessions))
	sids := make([]libacp.SessionID, 0, len(t.sessions))
	for sid, e := range t.sessions {
		entries = append(entries, e)
		sids = append(sids, sid)
		// Deregister from the shared permission router before dropping the map so
		// a shared engine stops routing approvals to this closing connection.
		t.deps.SessionRouter.unbind(e.InternalSessionID, t)
	}
	t.sessions = make(map[libacp.SessionID]*sessionEntry)
	t.contenoxToACPID = make(map[string]libacp.SessionID)
	t.sessionMu.Unlock()

	// A connection drop must stop any in-flight turns on this transport. libacp
	// cancels the prompt contexts it substituted when its own Run loop ends, but
	// the server owns cancellation here too, so it does not depend on that.
	for _, sid := range sids {
		t.cancelInflightPrompt(sid)
	}

	for _, e := range entries {
		t.cleanupMcpServers(ctx, store, e.McpServerNames)
		// Tear down this session's driver. For an external session this closes the
		// downstream agent it spawned; idempotent with the connCtx-cancel teardown
		// the New() Closed goroutine performs. Native is a no-op.
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
	row := t.deps.DB.WithoutTransaction().QueryRowContext(ctx, `
		SELECT mi.workspace_id
		FROM message_indices mi
		WHERE mi.name = $1 AND mi.identity = 'acp-client'
		ORDER BY (SELECT COUNT(*) FROM messages m WHERE m.idx_id = mi.id) DESC, mi.id DESC
		LIMIT 1`, name)
	var workspaceID string
	if err := row.Scan(&workspaceID); err != nil || workspaceID == "" {
		return "", false
	}
	return workspaceID, true
}

// listSessionsPageSize bounds one session/list page; a var so tests can
// exercise paging without minting hundreds of sessions.
var listSessionsPageSize = 100

// sessionListRow is one session/list candidate before pagination: the
// internal index id, the ACP session name, and the freshest message time
// (hasTime=false when the session has no messages yet).
type sessionListRow struct {
	internalID string
	name       string
	updatedAt  time.Time
	hasTime    bool
}

// sessionListRowLess is the freshest-first total order session/list returns:
// rows with a message time sort by it descending, rows without one sort after
// all rows that have one, and every tie falls back to internal id — the order
// must be total or the pagination cursor is ambiguous.
func sessionListRowLess(a, b sessionListRow) bool {
	if a.hasTime != b.hasTime {
		return a.hasTime
	}
	if a.hasTime && !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.internalID > b.internalID
}

// listSessionsCursor encodes a page boundary as the sort key of the last row
// the page returned: "<unixnano>|<internal id>", with an empty time part for
// rows that have no messages. Opaque to clients. Encoding the full sort key —
// not just the id — lets listSessionsResume position strictly after the
// boundary even when that row gained a fresher timestamp or was deleted
// between pages.
func listSessionsCursor(r sessionListRow) string {
	ts := ""
	if r.hasTime {
		ts = strconv.FormatInt(r.updatedAt.UnixNano(), 10)
	}
	return ts + "|" + r.internalID
}

// listSessionsResume returns the index of the first row sorting strictly
// after the cursor's boundary key, i.e. where the next page starts. A cursor
// that decodes to a key no longer present still positions correctly; a
// malformed cursor degrades to comparing by internal id alone.
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

	// The ACP session id is the message-index NAME (session/new mints it and
	// agentservice resolves loads by name); mi.id is contenox-internal. Rows
	// without a name predate ACP naming and cannot be loaded, so they are not
	// listed. Ordering and pagination happen in Go, not SQL: the roster must
	// come back freshest-first over MAX(added_at), but mi.id is a random UUID
	// (useless for ORDER BY) and the two schema dialects disagree on timestamp
	// representation, so a portable SQL keyset is not worth it. The
	// per-workspace roster is small; only the returned page pays the
	// title/cwd lookups. The cwd filter applies after pagination, so a
	// filtered page may carry fewer items but the cursor still advances.
	rows, err := exec.QueryContext(ctx, `
		SELECT mi.id, mi.name,
		       (SELECT MAX(m.added_at) FROM messages m WHERE m.idx_id = mi.id)
		FROM message_indices mi
		WHERE mi.workspace_id = $1
		  AND mi.identity = 'acp-client'
		  AND mi.name IS NOT NULL AND mi.name != ''`, t.workspaceID())
	if err != nil {
		return libacp.ListSessionsResponse{}, fmt.Errorf("acpsvc: list sessions: %w", err)
	}
	defer rows.Close()

	var all []sessionListRow
	for rows.Next() {
		var row sessionListRow
		var updatedAt any
		if err := rows.Scan(&row.internalID, &row.name, &updatedAt); err != nil {
			return libacp.ListSessionsResponse{}, fmt.Errorf("acpsvc: scan session: %w", err)
		}
		row.updatedAt, row.hasTime = parseDBTime(updatedAt)
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		return libacp.ListSessionsResponse{}, fmt.Errorf("acpsvc: rows: %w", err)
	}
	sort.Slice(all, func(i, j int) bool { return sessionListRowLess(all[i], all[j]) })

	start := 0
	if req.Cursor != "" {
		start = listSessionsResume(all, req.Cursor)
	}
	end := min(start+listSessionsPageSize, len(all))

	store := runtimetypes.New(exec)
	chatMgr := chatservice.NewManager(t.workspaceID())
	var sessions []libacp.SessionInfo
	for _, row := range all[start:end] {
		info := libacp.SessionInfo{
			SessionID: libacp.SessionID(row.name),
			Title:     t.sessionListTitle(ctx, chatMgr, exec, row.internalID, row.name),
			Cwd:       t.sessionCwd(ctx, store, libacp.SessionID(row.name)),
		}
		// Sessions carry their attribution in `_meta` so a client can tell what each
		// row IS: which registered agent runs an external session, and — for a
		// session a fleet dispatch created — which mission it is the unit of. Without
		// the mission half, a dispatched unit's session was indistinguishable from
		// the operator's own chats in beam's sidebar (same workspace, same identity),
		// which is exactly how a fired mission looked like it had "attached to an
		// existing session".
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

// sessionListTitle resolves a session/list Title in the fixed precedence
// every surface sees: an operator's own /rename first, then the derived
// "subject" heuristic, then the session name.
//
// The derived half is the same heuristic internalchatapi's chat listing used
// before it was retired in favor of ACP: the FIRST user message describes
// what the chat is about, unlike the last message which can be an assistant
// error or raw tool JSON. Falls back to the session name (fallback) when
// there is no stored user message yet, or on read failure — session/list must
// never error out over a title.
func (t *Transport) sessionListTitle(ctx context.Context, mgr *chatservice.Manager, exec libdb.Exec, internalSessionID, fallback string) string {
	if title := sessionTitleOverride(ctx, runtimetypes.New(exec), internalSessionID); title != "" {
		return title
	}
	if title := firstUserMessageTitle(ctx, mgr, exec, internalSessionID); title != "" {
		return title
	}
	return fallback
}

// firstUserMessageTitle derives a humane session title from the session's
// first non-empty, non-command-shaped user message, whitespace-collapsed and
// clipped to sessionListTitleMaxLen. Returns "" when the session has no such
// stored user message yet (including a session whose only user turns so far
// are commands) or on read failure — the shared heuristic behind both the
// session/list Title and the live post-turn session_info_update Title, so a
// client's tab/sidebar label matches whether it learned the title from a
// re-list or a live push.
//
// A command turn is skipped rather than titling the session: persistCommandTurn
// deliberately records a slash command's typed line ("/doctor") as an ordinary
// user message (see there), but that line is an instruction to the server, not
// a subject the operator would recognize their session by — a session whose
// first message happens to be "/doctor" must not be titled "/doctor" forever.
// The moment a real prose message arrives, this (and the live push driven by
// sessionInfoTitle) picks it up instead. See isCommandShapedText for the
// recognition reused to tell the two apart.
func firstUserMessageTitle(ctx context.Context, mgr *chatservice.Manager, exec libdb.Exec, internalSessionID string) string {
	msgs, err := mgr.ListMessages(ctx, exec, internalSessionID)
	if err != nil {
		return ""
	}
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" || isCommandShapedText(text) {
			continue
		}
		return truncateSessionListTitle(text)
	}
	return ""
}

// isCommandShapedText reports whether text is shaped like one of this
// server's slash commands — either a KNOWN one parseCommand recognizes
// ("/doctor", the case persistCommandTurn actually writes) or an
// unrecognized one that still has the shape (see unknownCommandName /
// commandShapeRE in commands.go: lowercase letters, digits and dashes as the
// leading token). It deliberately reuses both halves of the same recognition
// Prompt's own dispatch decision makes (see prompt.go) instead of re-deriving
// it, so a message firstUserMessageTitle treats as "a command, not a title"
// can never drift from what dispatch itself treats as a command.
//
// The shape test is what keeps a pasted path or prose that merely mentions a
// slash titling the session normally: "/etc/passwd contains x" has "etc/passwd"
// as its leading token, which fails commandShapeRE (a second slash), so it
// falls through to unknownCommandName's false and reads as an ordinary,
// legitimate title.
func isCommandShapedText(s string) bool {
	if _, _, ok := parseCommand(s); ok {
		return true
	}
	_, ok := unknownCommandName(s)
	return ok
}

// sessionInfoTitle resolves the live Title pushed on a prompt turn's
// session_info_update, in the SAME precedence as sessionListTitle (override,
// then the first-user-message heuristic) so a client's tab/sidebar label
// agrees whether it learned the title from a live push or a re-list. Empty
// when the session has neither (or when there is no DB to read) — callers omit
// the Title in that case so the notification stays a pure freshness
// (updatedAt) ping.
func (t *Transport) sessionInfoTitle(ctx context.Context, internalSessionID string) string {
	if t.deps.DB == nil || internalSessionID == "" {
		return ""
	}
	exec := t.deps.DB.WithoutTransaction()
	if title := sessionTitleOverride(ctx, runtimetypes.New(exec), internalSessionID); title != "" {
		return title
	}
	mgr := chatservice.NewManager(t.workspaceID())
	return firstUserMessageTitle(ctx, mgr, exec, internalSessionID)
}

// acpSessionTitleKVPrefix namespaces the operator's own session titles. It is
// keyed by the INTERNAL session id, not the ACP session id, for the same
// reason mi.id is the durable identity everywhere else: the ACP id is the
// message-index NAME, and a title must not become unreachable if that name is
// ever re-minted.
//
// A title is stored SEPARATELY from the name rather than by renaming the
// message index, because in ACP the name IS the session id (see newSessionID):
// renaming it would silently break every stored reference to the session —
// which is exactly why messagestore.RenameSession has no ACP caller.
const acpSessionTitleKVPrefix = "acp:session_title:"

type sessionTitleRecord struct {
	Title string `json:"title"`
}

// setSessionTitleOverride stores (or, on an empty title, clears) the
// operator's own title for a session. Clearing is not a deletion of the
// concept — it hands the label back to the derived heuristic, which is the
// only sane "reset" a title can have.
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
// when they never set one. A read failure is "" too: a title is a label, and
// no label is worth failing a session listing over.
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
// report it (the spec requires cwd on SessionInfo) and filter by it across
// process restarts — the in-memory session map is empty in a fresh process.
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

// parseDBTime normalizes MAX(added_at) across drivers: SQLite hands back
// strings (layout depends on how the value was written), Postgres a time.Time.
func parseDBTime(v any) (time.Time, bool) {
	switch tv := v.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return tv, true
	case []byte:
		return parseDBTimeString(string(tv))
	case string:
		return parseDBTimeString(tv)
	}
	return time.Time{}, false
}

func parseDBTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		// time.Time.String() — what the sqlite driver stores when a time.Time
		// is bound to a TIMESTAMP column. Until this layout was handled, every
		// session/list row lost its updatedAt and the sidebar sort collapsed
		// to random-UUID order.
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
