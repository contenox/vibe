package taskengine

import (
	"context"
	"encoding/json"
	"time"

	libkv "github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
)

const (
	kvEventsPrefix     = "taskevents:"
	kvEventsMaxEntries = 500
	// Large text fields are capped so a single run cannot bloat the KV store;
	// the live SSE stream still carries the full values.
	journalTextFieldCap = 16 * 1024
)

// KVJournalTaskEventSink journals task events durably per request ID into the
// KV store (alongside the CapturedStateUnit stream the KVInspector persists),
// after forwarding them to the wrapped sink. This is what makes a run's work
// log — tool calls, diffs, approvals — re-renderable after the 5-minute bus
// TTL and across restarts.
//
// step_chunk events are deliberately not journaled: they are streaming detail
// whose final text is already persisted with the chat history.
// step_stream_end is journaled — the durable stream bracket (chunk count,
// finish reason, usage) — so a replayed run can tell streaming happened and
// how it ended even though the chunks themselves are gone. Every other kind
// is journaled.
type KVJournalTaskEventSink struct {
	inner   TaskEventSink
	kv      libkv.KVManager
	tracker libtracker.ActivityTracker
}

func NewKVJournalTaskEventSink(inner TaskEventSink, kv libkv.KVManager, tracker libtracker.ActivityTracker) *KVJournalTaskEventSink {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &KVJournalTaskEventSink{inner: inner, kv: kv, tracker: tracker}
}

// Wants defers to the wrapped sink (matching the pre-Wants Enabled()
// delegation); with no inner sink the journal itself consumes every kind.
func (s *KVJournalTaskEventSink) Wants(kind TaskEventKind) bool {
	if s.inner != nil {
		return s.inner.Wants(kind)
	}
	return true
}

func (s *KVJournalTaskEventSink) PublishTaskEvent(ctx context.Context, event TaskEvent) error {
	var innerErr error
	if s.inner != nil {
		innerErr = s.inner.PublishTaskEvent(ctx, event)
	}

	if event.RequestID == "" || event.Kind == TaskEventStepChunk {
		return innerErr
	}

	reportErr, _, end := s.tracker.Start(ctx, "journal_event", "taskevents_kv",
		"request_id", event.RequestID, "kind", string(event.Kind))
	defer end()

	data, err := json.Marshal(sanitizeTaskEventForJournal(event))
	if err != nil {
		reportErr(err)
		return innerErr
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	op, err := s.kv.Executor(writeCtx)
	if err != nil {
		reportErr(err)
		return innerErr
	}

	key := kvEventsPrefix + event.RequestID
	if err := op.ListPush(writeCtx, key, data); err != nil {
		reportErr(err)
		return innerErr
	}
	n, err := op.ListLength(writeCtx, key)
	if err != nil {
		reportErr(err)
		return innerErr
	}
	if n > kvEventsMaxEntries {
		if err := op.ListTrim(writeCtx, key, 0, kvEventsMaxEntries-1); err != nil {
			reportErr(err)
		}
	}
	return innerErr
}

// GetJournaledEvents returns the durably journaled events of a run, in
// arrival order. A request with no journal yields an empty slice.
func GetJournaledEvents(ctx context.Context, kv libkv.KVManager, reqID string) ([]TaskEvent, error) {
	if reqID == "" {
		return nil, nil
	}
	op, err := kv.Executor(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := op.ListRange(ctx, kvEventsPrefix+reqID, 0, -1)
	if err != nil {
		return nil, err
	}
	out := make([]TaskEvent, 0, len(raw))
	for _, b := range raw {
		var ev TaskEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	// ListPush prepends (LPUSH); reverse so callers get arrival order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func sanitizeTaskEventForJournal(ev TaskEvent) TaskEvent {
	ev.Content = capJournalText(ev.Content)
	ev.Thinking = capJournalText(ev.Thinking)
	ev.ApprovalDiff = capJournalText(ev.ApprovalDiff)
	ev.ToolDiffOldText = capJournalText(ev.ToolDiffOldText)
	ev.ToolDiffNewText = capJournalText(ev.ToolDiffNewText)
	return ev
}

func capJournalText(s string) string {
	if len(s) <= journalTextFieldCap {
		return s
	}
	return s[:journalTextFieldCap] + "\n…[truncated]"
}
