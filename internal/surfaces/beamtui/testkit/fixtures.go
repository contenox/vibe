// This file is the versioned fixture corpus (blueprint 4.21): scripted
// enginebridge.Event sequences that every beam component's tests replay
// instead of hand-rolling their own event lists. Each function is a pure,
// deterministic constructor — fixed ids, fixed strings, no time.Now, no
// randomness — so two calls with the same sessionID produce byte-identical
// output and a golden test built from one stays reproducible forever.
//
// The shapes are derived from the real wire producers: internal/surfaces/
// acpsvc/events.go (which turns taskengine.TaskEvent into libacp.
// SessionNotification) and internal/kernel/taskengine/events.go (the engine's
// own event vocabulary) — tool-call ids, titles and kinds here follow the
// same conventions acpsvc actually emits (see toolCallTitle, toolKindFor,
// diffContentFromEvent), and mission fixtures follow the missionReportMeta /
// missionAskMeta envelope enginebridge.translate decodes.
package testkit

import (
	"encoding/json"

	"github.com/contenox/beam/internal/services/approvalflow"
	"github.com/contenox/beam/internal/surfaces/beamtui/enginebridge"
	libacp "github.com/contenox/beam/libacp"
)

// FixtureStreamingTurn scripts a whole ordinary turn: the user's message
// echoed back, assistant prose streamed in chunks that split mid-word (the
// shape a real model stream actually produces, and the case that breaks a
// renderer which assumes chunks land on word boundaries), a file-editing
// tool call opening and completing, more prose, a usage update and the
// turn's terminal StopReason. It is the default "does the transcript render
// a normal turn" fixture.
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
		// Mid-word splits: "I'll" and "loop" are both cut across chunks, the
		// shape that breaks a renderer assuming chunks land on word boundaries.
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

// FixtureTwoTurnsEmptyMessageID scripts two ordinary turns the way the REAL
// wire scripts them: with no MessageID on anything.
//
// MessageID is optional in the ACP spec and in practice absent — nothing in
// libacp stamps one on an agent_message_chunk (see libacp.NewAgentMessageChunk,
// which carries content and nothing else) — so every assistant message in a
// process arrives under the same empty id. A consumer that keys "this message
// has ended" on the id alone therefore ends ALL of them on the first
// TurnEnded, and every later turn's prose disappears while the tool cards, the
// context gauge and the spinner keep moving. That was the first dogfooding
// hunt's blocker, and this fixture is the shape that reproduces it: both turns
// have to render.
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

// FixtureStreamingTurnEmptyMessageID is FixtureStreamingTurn with the ids the
// wire does not actually send: same turn, same tool call, same mid-word chunk
// splits, but every message-carrying event unidentified. Replaying both
// against the same consumer is how a suite pins that grouping degrades to
// "the stream is the message" instead of collapsing.
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

// FixtureReplayedHistory scripts what a session/load replay puts on the wire:
// past turns re-delivered as ordinary user/agent chunks, unidentified like
// everything else, and then NOTHING — no TurnEnded, no end-of-history event of
// any kind. The replay simply stops.
//
// It is the fixture for the two defects that shape hides. A consumer that only
// ends a message on TurnEnded leaves the last replayed message open forever
// (and then prints everything it settles afterwards ABOVE it, since settled
// history is by definition above a live region); a consumer that also ends one
// on the next UserEcho gets every turn but the last, which is why a replay
// needs an explicit end from whoever knows the load is finished.
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

// FixtureMissionReportMidStream scripts a MissionReport landing in the
// middle of an unrelated live turn's own text stream — the race the
// blueprint's cross-component contract names explicitly ("a report card
// racing a live stream"): the live turn's TextDelta chunks and the mission
// report's chunk share the session but carry DISTINCT MessageIDs, which is
// exactly what lets a transcript tell them apart and render two separate
// entries instead of interleaving one message's text.
func FixtureMissionReportMidStream(sessionID libacp.SessionID) []enginebridge.Event {
	const liveMsg = "msg-live-1"
	// mission-report-<reportId> mirrors enginebridge's translate: on the wire
	// this MessageID is what a real agent_message_chunk carrying a
	// missionReportMeta envelope actually uses.
	const reportMsg = "mission-report-report-42"
	return []enginebridge.Event{
		enginebridge.TextDelta{SessionID: sessionID, MessageID: liveMsg, Text: "Running the mig"},
		enginebridge.TextDelta{SessionID: sessionID, MessageID: liveMsg, Text: "ration script now..."},
		enginebridge.MissionReport{
			SessionID: sessionID,
			MissionID: "mission-7",
			ReportID:  "report-42",
			// "progress" mirrors missionservice.ReportKindProgress; duplicated as
			// a literal here for the same reason enginebridge duplicates the
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

// FixtureMissionHeartbeat scripts the maintainer's other named liveness
// case (blueprint 4.7: "mission fire-and-detach heartbeat"): a dispatched
// mission unit stays active — a tool call opens, is bumped, and completes,
// with mission progress pings alongside it — and NOT ONE event carries
// visible text (every TextDelta/ThoughtDelta/UserEcho is absent; the
// MissionReport pings' Text is empty, a silent heartbeat). This is the
// fixture that catches a liveness implementation which only pulses on
// streamed prose: activity here is real and ongoing, but there is nothing
// to print except the spinner.
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

// FixtureMissionLifecycle scripts a dispatched mission's whole visible life as
// it lands in the session that FIRED it: the mission opening, its planner
// replacing the plan once as the work takes shape, and the mission coming to
// rest as landed with the one-line reason the finisher gave.
//
// It is built to be one fixture and two opposite answers, because that is what
// the bell rules need pinned: opening a mission and revising a plan are the
// unit getting on with the work it was given and must NEVER ring, while
// reaching a terminal status is the moment detached work has something to hand
// back and rings under the ordinary focus rule. A suite that replayed only the
// terminal event could not tell a correct implementation from one that rings on
// everything mission-shaped.
//
// The counts follow a plan that is actually progressing (2 done, 1 running,
// 1 pending of four entries), so a renderer's counts line has three distinct
// non-zero numbers rather than a row of zeroes that would hide a field mix-up.
func FixtureMissionLifecycle(sessionID libacp.SessionID) []enginebridge.Event {
	const missionID = "mission-19"
	return []enginebridge.Event{
		enginebridge.MissionStatusChanged{
			SessionID: sessionID,
			MissionID: missionID,
			AgentName: "porter",
			// "" -> "open": a mission has no prior state when it is created,
			// which is the shape the status vocabulary actually produces.
			Old: "",
			New: enginebridge.MissionStatusOpen,
			// mission-status-<missionId>-<old>-<new> mirrors the MessageID the
			// report router stamps (reportrouter.statusMessageID): the
			// discriminator is the TRANSITION, not a counter, so a redelivery
			// off the at-least-once bus collapses onto the same message id
			// instead of rendering the same landing twice.
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

// FixtureInboxArrival scripts an operator-inbox arrival: a mission report that
// reached NO live supervising session and was written to the durable inbox
// instead.
//
// It takes NO session id, unlike every other fixture in this corpus, and that
// is the fixture's whole point rather than an inconsistency. An inbox item
// exists because there was no session to deliver it into — enginebridge.
// InboxItemAdded.SessionOf is the empty id by construction — so a constructor
// that accepted one would be inviting a caller to assume a routing edge that
// does not exist. It is excluded from allFixtures() in the tests for the same
// reason: that inventory asserts every event carries the session it was built
// for, which this one cannot and must not.
//
// The two items cover both reasons the router distinguishes: a mission an
// operator fired directly (no supervisor was ever intended) and one whose
// parent session had already ended (a supervisor was intended and missed).
func FixtureInboxArrival() []enginebridge.Event {
	return []enginebridge.Event{
		enginebridge.InboxItemAdded{
			ID:        "inbox-1",
			MissionID: "mission-23",
			AgentName: "auditor",
			Intent:    "audit the dependency licences",
			// "operator_fired" mirrors operatorinbox.ReasonOperatorFired;
			// duplicated as a literal for the same reason the mission report
			// kinds are (see FixtureMissionReportMidStream).
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
// (including a small unified-shaped diff a card renders), the call resumes
// to in_progress once the operator resolves it (there is no distinct
// "resolved" wire event — the resume itself, the tool call continuing past
// the gate, IS the observable fact), and finally completes.
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
			// Resolve is a no-op here: fixtures are inert scripted data, not a
			// live gate. A caller that needs to observe or drive the resolve
			// path should use FakeBridge instead.
			Resolve: func(bool) {},
		},
		// The gate resolved: the same tool call resumes past pending. This
		// transition IS "resolved" on the wire — there is no separate event.
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
			Chunk:     "\x1b[32mok\x1b[0m  \tgithub.com/contenox/beam/internal/ingest\t0.412s\n",
			Reset:     false,
		},
		enginebridge.ShellRunResult{SessionID: sessionID, Offset: int64(len(snapshot)) + 60, Started: true},
	}
}

// FixtureContextPressure scripts a session's usage_update sequence crossing
// both context-budget thresholds the blueprint names (D-level 75%/90%
// warnings): three UsageUpdated events at roughly 50%, 76% and 91% of a
// 100000-token budget, the last one carrying a UsageCost the way a
// cost-aware provider actually reports it.
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
