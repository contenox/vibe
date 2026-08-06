// This file is the versioned fixture corpus: scripted enginebridge.Event
// sequences that component tests replay instead of hand-rolling their own.
// Each function is a pure, deterministic constructor — fixed ids, no
// time.Now, no randomness — so a given sessionID always produces the same
// output. Shapes mirror the real wire producers (internal/surfaces/acpsvc/
// events.go, internal/kernel/taskengine/events.go).
package testkit

import (
	"encoding/json"

	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/surfaces/beamtui/enginebridge"
	libacp "github.com/contenox/contenox/libacp"
)

// FixtureStreamingTurn scripts a whole ordinary turn: user message echoed
// back, assistant prose streamed in chunks that split mid-word (the shape a
// real model stream produces), a file-editing tool call opening and
// completing, more prose, a usage update, and the turn's terminal
// StopReason. It is the default "normal turn renders" fixture.
func FixtureStreamingTurn(sessionID libacp.SessionID) []enginebridge.Event {
	const (
		userMsg      = "msg-user-1"
		assistantMsg = "msg-assistant-1"
		followUpMsg  = "msg-assistant-2"
		toolCallID   = "local_fs.write_file#1"
	)
	return []enginebridge.Event{
		enginebridge.UserEcho{
			SessionID: sessionID,
			MessageID: userMsg,
			Text:      "Add exponential backoff to the retry loop in the ingest worker.",
		},
		// "I'll" and "loop" are split mid-word across chunks.
		enginebridge.TextDelta{SessionID: sessionID, MessageID: assistantMsg, Text: "I'l"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: assistantMsg, Text: "l add exponential backoff to the retry lo"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: assistantMsg, Text: "op now."},
		enginebridge.ToolCallOpened{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			Title:      "write_file: internal/ingest/retry.go",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusInProgress,
			RawInput:   json.RawMessage(`{"path":"internal/ingest/retry.go"}`),
		},
		enginebridge.ToolCallUpdated{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			Title:      "write_file: internal/ingest/retry.go",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusCompleted,
			Contents: []libacp.ToolCallContent{
				{Type: libacp.ToolCallContentDiff, Path: "internal/ingest/retry.go",
					OldText: "const retryDelay = 5 * time.Second\n",
					NewText: "const maxRetryDelay = 30 * time.Second\n"},
			},
			RawOutput: json.RawMessage(`{"path":"internal/ingest/retry.go","written":true}`),
		},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: followUpMsg, Text: "Back"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: followUpMsg, Text: "off is now capped at 30s with jitter."},
		enginebridge.UsageUpdated{SessionID: sessionID, Used: 1200, Size: 128000},
		enginebridge.TurnEnded{SessionID: sessionID, StopReason: libacp.StopReasonEndTurn},
	}
}

// FixtureTwoTurnsEmptyMessageID scripts two ordinary turns the way the wire
// actually does: no MessageID on anything, since libacp never stamps one on
// an agent_message_chunk. A consumer that ends "this message" on the id
// alone therefore ends every assistant message on the first TurnEnded, so
// later turns' prose disappears while tool cards and the spinner keep
// moving; both turns must render here to catch that regression.
func FixtureTwoTurnsEmptyMessageID(sessionID libacp.SessionID) []enginebridge.Event {
	return []enginebridge.Event{
		enginebridge.UserEcho{SessionID: sessionID, Text: "What does the ingest worker retry on?"},
		enginebridge.TextDelta{SessionID: sessionID, Text: "It retries on 429 and on any 5"},
		enginebridge.TextDelta{SessionID: sessionID, Text: "xx from the upstream API.\n"},
		enginebridge.TurnEnded{SessionID: sessionID, StopReason: libacp.StopReasonEndTurn},

		enginebridge.UserEcho{SessionID: sessionID, Text: "And on a timeout?"},
		enginebridge.TextDelta{SessionID: sessionID, Text: "Timeouts are retried too, with the sa"},
		enginebridge.TextDelta{SessionID: sessionID, Text: "me backoff.\n"},
		enginebridge.TurnEnded{SessionID: sessionID, StopReason: libacp.StopReasonEndTurn},
	}
}

// FixtureStreamingTurnEmptyMessageID is FixtureStreamingTurn with every
// message-carrying event unidentified, matching what the wire actually
// sends. Replaying both against the same consumer pins that grouping
// degrades to "the stream is the message" instead of collapsing.
func FixtureStreamingTurnEmptyMessageID(sessionID libacp.SessionID) []enginebridge.Event {
	out := FixtureStreamingTurn(sessionID)
	for i, e := range out {
		switch ev := e.(type) {
		case enginebridge.UserEcho:
			ev.MessageID = ""
			out[i] = ev
		case enginebridge.TextDelta:
			ev.MessageID = ""
			out[i] = ev
		case enginebridge.ThoughtDelta:
			ev.MessageID = ""
			out[i] = ev
		}
	}
	return out
}

// FixtureReplayedHistory scripts a session/load replay: past turns
// re-delivered as unidentified user/agent chunks, then nothing — no
// TurnEnded, no end-of-history event. It catches two defects: ending a
// message only on TurnEnded leaves the last replayed message open forever,
// and ending one on the next UserEcho instead loses the true last turn — a
// replay needs an explicit end from whoever knows the load finished.
func FixtureReplayedHistory(sessionID libacp.SessionID) []enginebridge.Event {
	return []enginebridge.Event{
		enginebridge.UserEcho{SessionID: sessionID, Text: "Summarise the ingest worker."},
		enginebridge.TextDelta{SessionID: sessionID, Text: "It pulls batches off the queue and writes them through the store."},
		enginebridge.UserEcho{SessionID: sessionID, Text: "Where does it retry?"},
		enginebridge.TextDelta{SessionID: sessionID, Text: "In the write path, with a fixed 5s delay."},
	}
}

// FixtureThoughtThenAnswer scripts a reasoning-then-prose turn: thought
// chunks (agent_thought_chunk) followed by the answer they lead to. It is
// the fixture for anything that renders thought traces distinctly from
// assistant prose (frame.StyleThought vs frame.StyleAssistant).
func FixtureThoughtThenAnswer(sessionID libacp.SessionID) []enginebridge.Event {
	const (
		thoughtMsg   = "msg-thought-1"
		assistantMsg = "msg-assistant-1"
	)
	return []enginebridge.Event{
		enginebridge.UserEcho{SessionID: sessionID, MessageID: "msg-user-1", Text: "Why does the retry loop back off so slowly?"},
		enginebridge.ThoughtDelta{SessionID: sessionID, MessageID: thoughtMsg, Text: "Let me check how the existing retry"},
		enginebridge.ThoughtDelta{SessionID: sessionID, MessageID: thoughtMsg, Text: " loop backs off before answering."},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: assistantMsg, Text: "The retry loop currently uses a fixed 5s de"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: assistantMsg, Text: "lay; switching to exponential backoff would help."},
		enginebridge.TurnEnded{SessionID: sessionID, StopReason: libacp.StopReasonEndTurn},
	}
}

// FixtureMissionReportMidStream scripts a MissionReport landing mid-stream
// of an unrelated live turn: both share the session but carry distinct
// MessageIDs, which is what lets a transcript render them as two separate
// entries instead of interleaving one message's text.
func FixtureMissionReportMidStream(sessionID libacp.SessionID) []enginebridge.Event {
	const liveMsg = "msg-live-1"
	// mission-report-<reportId> mirrors the MessageID enginebridge.translate
	// derives from a real missionReportMeta envelope.
	const reportMsg = "mission-report-report-42"
	return []enginebridge.Event{
		enginebridge.TextDelta{SessionID: sessionID, MessageID: liveMsg, Text: "Running the mig"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: liveMsg, Text: "ration script now..."},
		enginebridge.MissionReport{
			SessionID: sessionID,
			MissionID: "mission-7",
			ReportID:  "report-42",
			// "progress" mirrors missionservice.ReportKindProgress, duplicated as
			// a literal for the same reason enginebridge duplicates the
			// report-router's _meta keys (see enginebridge/events.go).
			Kind:      "progress",
			AgentName: "scout",
			MessageID: reportMsg,
			Text:      "Indexed 1,204 files across the repo.",
		},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: liveMsg, Text: " Migration finished; 3 tables updated."},
		enginebridge.TurnEnded{SessionID: sessionID, StopReason: libacp.StopReasonEndTurn},
	}
}

// FixtureMissionHeartbeat scripts a dispatched mission staying active — a
// tool call opens, is bumped, and completes, with mission progress pings
// alongside it — while no event carries visible text (MissionReport pings'
// Text is empty, a silent heartbeat). It catches a liveness implementation
// that only pulses on streamed prose.
func FixtureMissionHeartbeat(sessionID libacp.SessionID) []enginebridge.Event {
	const toolCallID = "mission.dispatch#1"
	return []enginebridge.Event{
		enginebridge.ToolCallOpened{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			Title:      "mission: audit-dependencies",
			Kind:       libacp.ToolKindExecute,
			Status:     libacp.ToolCallStatusInProgress,
		},
		enginebridge.MissionReport{
			SessionID: sessionID, MissionID: "mission-11", ReportID: "hb-1",
			Kind: "progress", AgentName: "auditor", MessageID: "mission-report-hb-1", Text: "",
		},
		enginebridge.ToolCallUpdated{
			SessionID: sessionID, ToolCallID: toolCallID,
			Title: "mission: audit-dependencies", Kind: libacp.ToolKindExecute,
			Status: libacp.ToolCallStatusInProgress,
		},
		enginebridge.MissionReport{
			SessionID: sessionID, MissionID: "mission-11", ReportID: "hb-2",
			Kind: "progress", AgentName: "auditor", MessageID: "mission-report-hb-2", Text: "",
		},
		enginebridge.ToolCallUpdated{
			SessionID: sessionID, ToolCallID: toolCallID,
			Title: "mission: audit-dependencies", Kind: libacp.ToolKindExecute,
			Status: libacp.ToolCallStatusCompleted,
		},
		enginebridge.MissionReport{
			SessionID: sessionID, MissionID: "mission-11", ReportID: "hb-3",
			Kind: "result", AgentName: "auditor", MessageID: "mission-report-hb-3", Text: "",
		},
	}
}

// FixtureMissionLifecycle scripts a dispatched mission's whole visible life
// in the session that fired it: opening, a plan revision, then landing with
// a one-line reason. It pins the bell rule — opening/revising must never
// ring, only a terminal status rings — and uses mixed, nonzero counts (2
// done, 1 running, 1 pending) so a field mix-up can't hide behind zeroes.
func FixtureMissionLifecycle(sessionID libacp.SessionID) []enginebridge.Event {
	const missionID = "mission-19"
	return []enginebridge.Event{
		enginebridge.MissionStatusChanged{
			SessionID: sessionID,
			MissionID: missionID,
			AgentName: "porter",
			// "" -> "open": a mission has no prior state at creation.
			Old: "",
			New: enginebridge.MissionStatusOpen,
			// mission-status-<missionId>-<old>-<new> mirrors reportrouter.
			// statusMessageID, keyed on the transition so an at-least-once
			// redelivery collapses onto the same id instead of rendering the
			// landing twice.
			MessageID: "mission-status-" + missionID + "--open",
		},
		enginebridge.MissionPlanRevised{
			SessionID:   sessionID,
			MissionID:   missionID,
			AgentName:   "porter",
			Revision:    2,
			Explanation: "split the migration step now that the schema is known",
			EntryCount:  4,
			Pending:     1,
			InProgress:  1,
			Completed:   2,
			MessageID:   "mission-plan-" + missionID + "-2",
		},
		enginebridge.MissionStatusChanged{
			SessionID: sessionID,
			MissionID: missionID,
			AgentName: "porter",
			Old:       enginebridge.MissionStatusOpen,
			New:       enginebridge.MissionStatusLanded,
			Reason:    "migration applied; 3 tables updated",
			MessageID: "mission-status-" + missionID + "-open-landed",
		},
	}
}

// FixtureInboxArrival scripts an operator-inbox arrival: a mission report
// that reached no live supervising session and was written to the durable
// inbox instead. It takes no session id, unlike every other fixture here —
// InboxItemAdded.SessionOf is empty by construction — and is excluded from
// allFixtures() since that inventory asserts every event carries its
// session. The two items cover both reasons the router distinguishes: an
// operator-fired mission, and one whose parent session had ended.
func FixtureInboxArrival() []enginebridge.Event {
	return []enginebridge.Event{
		enginebridge.InboxItemAdded{
			ID:        "inbox-1",
			MissionID: "mission-23",
			AgentName: "auditor",
			Intent:    "audit the dependency licences",
			// "operator_fired" mirrors operatorinbox.ReasonOperatorFired (see
			// FixtureMissionReportMidStream for why it's a duplicated literal).
			Reason:  "operator_fired",
			Kind:    "result",
			Summary: "4 packages carry a copyleft licence; list attached.",
		},
		enginebridge.InboxItemAdded{
			ID:        "inbox-2",
			MissionID: "mission-24",
			AgentName: "scout",
			Intent:    "trace the retry path",
			// "parent_gone" mirrors operatorinbox.ReasonParentGone.
			Reason:  "parent_gone",
			Kind:    "blocker",
			Summary: "the ingest worker has two retry loops; which one?",
		},
	}
}

// FixtureApprovalFlow scripts a HITL gate end to end: a tool call opens
// pending, a PermissionRequested blocks it with approvalflow's decoded Meta
// (including a small diff a card renders), the call resumes to in_progress
// once the operator resolves it — there is no distinct "resolved" wire
// event, the resume is the observable fact — and finally completes.
func FixtureApprovalFlow(sessionID libacp.SessionID) []enginebridge.Event {
	const toolCallID = "local_fs.write_file#2"
	diff := []libacp.ToolCallContent{
		{Type: libacp.ToolCallContentDiff, Path: "internal/config/limits.go",
			OldText: "const MaxRetries = 3\n",
			NewText: "const MaxRetries = 5\n"},
	}
	options := []libacp.PermissionOption{
		{OptionID: approvalflow.OptionAllow, Name: "Allow", Kind: libacp.PermissionAllowOnce},
		{OptionID: approvalflow.OptionDeny, Name: "Deny", Kind: libacp.PermissionRejectOnce},
	}
	return []enginebridge.Event{
		enginebridge.ToolCallOpened{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			Title:      "write_file: internal/config/limits.go",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusPending,
			RawInput:   json.RawMessage(`{"path":"internal/config/limits.go"}`),
		},
		enginebridge.PermissionRequested{
			SessionID:  sessionID,
			ToolCallID: toolCallID,
			Title:      "write_file: internal/config/limits.go",
			Kind:       libacp.ToolKindEdit,
			Status:     libacp.ToolCallStatusPending,
			Meta: approvalflow.Meta{
				ToolsName:  "local_fs",
				ToolName:   "write_file",
				PolicyName: "default",
				PolicyPath: ".contenox/policies/default.yaml",
				DiffOld:    "const MaxRetries = 3\n",
				DiffNew:    "const MaxRetries = 5\n",
			},
			Contents: diff,
			RawInput: json.RawMessage(`{"path":"internal/config/limits.go"}`),
			Options:  options,
			// Resolve is a no-op: fixtures are inert scripted data, not a live
			// gate; use FakeBridge to drive the resolve path.
			Resolve: func(bool) {},
		},
		// The gate resolved: the same tool call resumes past pending — there
		// is no separate "resolved" event on the wire.
		enginebridge.ToolCallUpdated{
			SessionID: sessionID, ToolCallID: toolCallID,
			Title: "write_file: internal/config/limits.go", Kind: libacp.ToolKindEdit,
			Status: libacp.ToolCallStatusInProgress,
		},
		enginebridge.ToolCallUpdated{
			SessionID: sessionID, ToolCallID: toolCallID,
			Title: "write_file: internal/config/limits.go", Kind: libacp.ToolKindEdit,
			Status:    libacp.ToolCallStatusCompleted,
			Contents:  diff,
			RawOutput: json.RawMessage(`{"path":"internal/config/limits.go","written":true}`),
		},
	}
}

// FixtureShellRun scripts an operator `$`-passthrough line: ShellRunStarted,
// then TerminalChunk output including one chunk with embedded ANSI SGR
// codes (real shell output is not plain text) and one Reset chunk (the
// scrollback-replacing snapshot a (re)subscribe delivers), and the run's
// result.
func FixtureShellRun(sessionID libacp.SessionID) []enginebridge.Event {
	const cmd = "go test ./internal/ingest/... -run TestRetry"
	snapshot := "$ " + cmd + "\n"
	return []enginebridge.Event{
		enginebridge.ShellRunStarted{SessionID: sessionID, Command: cmd},
		// Reset: true — replaces the consumer's buffer, never appends.
		enginebridge.TerminalChunk{SessionID: sessionID, Offset: 0, Chunk: snapshot, Reset: true},
		// Embedded ANSI: a green "ok" the real `go test` shell would print.
		enginebridge.TerminalChunk{
			SessionID: sessionID,
			Offset:    int64(len(snapshot)),
			Chunk:     "\x1b[32mok\x1b[0m  \tgithub.com/contenox/contenox/internal/ingest\t0.412s\n",
			Reset:     false,
		},
		enginebridge.ShellRunResult{SessionID: sessionID, Offset: int64(len(snapshot)) + 60, Started: true},
	}
}

// FixtureContextPressure scripts a session's usage_update sequence crossing
// both context-budget warning thresholds: three UsageUpdated events at
// roughly 50%, 76% and 91% of a 100000-token budget, the last carrying a
// UsageCost the way a cost-aware provider actually reports it.
func FixtureContextPressure(sessionID libacp.SessionID) []enginebridge.Event {
	const size = 100000
	return []enginebridge.Event{
		enginebridge.UsageUpdated{SessionID: sessionID, Used: 50000, Size: size},
		enginebridge.UsageUpdated{SessionID: sessionID, Used: 76000, Size: size},
		enginebridge.UsageUpdated{
			SessionID: sessionID, Used: 91000, Size: size,
			Cost: &libacp.UsageCost{Amount: 0.42, Currency: "USD"},
		},
	}
}
