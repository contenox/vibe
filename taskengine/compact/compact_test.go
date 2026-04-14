package compact

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func mkMsg(role, content string) Message { return Message{Role: role, Content: content} }

func charCount(_ context.Context, _, s string) (int, error) {
	// Pretend each character is one token to make thresholds deterministic.
	return len(s), nil
}

func okCaller(reply string) Caller {
	return func(_ context.Context, _, _, _ string) (string, error) { return reply, nil }
}

func failCaller(err error) Caller {
	return func(_ context.Context, _, _, _ string) (string, error) { return "", err }
}

// applySplice mirrors what taskengine wiring does: replace [from, to) with one
// synthetic message preserving leading-system and trailing tail.
func applySplice(msgs []Message, r Result) []Message {
	if !r.Compacted {
		return msgs
	}
	out := make([]Message, 0, r.ReplaceFrom+1+len(msgs)-r.ReplaceTo)
	out = append(out, msgs[:r.ReplaceFrom]...)
	out = append(out, Message{Role: "user", Content: r.SyntheticContent})
	out = append(out, msgs[r.ReplaceTo:]...)
	return out
}

func TestMaybe_NoOpUnderThreshold(t *testing.T) {
	msgs := []Message{mkMsg("user", "hi"), mkMsg("assistant", "hello")}
	res, err := Maybe(context.Background(), Policy{}, &State{}, msgs, 1000, charCount, okCaller("nope"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Compacted {
		t.Fatalf("should not compact under threshold: %+v", res)
	}
}

func TestMaybe_NoOpWhenHistoryShort(t *testing.T) {
	msgs := []Message{mkMsg("user", strings.Repeat("x", 1000))}
	res, _ := Maybe(context.Background(), Policy{KeepRecent: 10, MinReplacedMessages: 4}, &State{}, msgs, 100, charCount, okCaller("s"))
	if res.Compacted {
		t.Fatalf("should not compact short history")
	}
}

func TestMaybe_CompactsAndReturnsSplice(t *testing.T) {
	// 20 messages, each 50 chars → 1000 "tokens". Threshold 0.5 → 500.
	msgs := make([]Message, 20)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = mkMsg(role, strings.Repeat("a", 50))
	}
	st := &State{}
	res, err := Maybe(context.Background(),
		Policy{TriggerFraction: 0.5, KeepRecent: 4, MinReplacedMessages: 4},
		st, msgs, 1000, charCount, okCaller("compacted summary text"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected compaction: %+v", res)
	}
	if res.ReplaceFrom != 0 || res.ReplaceTo != 16 || res.Replaced != 16 {
		t.Fatalf("indices wrong: from=%d to=%d replaced=%d", res.ReplaceFrom, res.ReplaceTo, res.Replaced)
	}
	if !strings.Contains(res.SyntheticContent, "<compact-summary>") {
		t.Fatalf("synthetic content not wrapped: %q", res.SyntheticContent)
	}
	out := applySplice(msgs, res)
	if len(out) != 5 {
		t.Fatalf("expected 1 synthetic + 4 kept = 5, got %d", len(out))
	}
	failures, compactions, disabled, _ := st.Snapshot()
	if failures != 0 || compactions != 1 || disabled {
		t.Fatalf("state unexpected: failures=%d compactions=%d disabled=%v", failures, compactions, disabled)
	}
}

func TestMaybe_PreservesLeadingSystem(t *testing.T) {
	msgs := []Message{mkMsg("system", strings.Repeat("S", 100))}
	for i := 0; i < 30; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, mkMsg(role, strings.Repeat("a", 50)))
	}
	res, err := Maybe(context.Background(),
		Policy{TriggerFraction: 0.1, KeepRecent: 4, MinReplacedMessages: 4},
		&State{}, msgs, 1000, charCount, okCaller("summary"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected compaction: %+v", res)
	}
	if res.ReplaceFrom != 1 {
		t.Fatalf("must skip leading system; ReplaceFrom=%d", res.ReplaceFrom)
	}
	out := applySplice(msgs, res)
	if out[0].Role != "system" {
		t.Fatalf("system message not preserved at index 0; got role=%q", out[0].Role)
	}
	if !strings.Contains(out[1].Content, "<compact-summary>") {
		t.Fatalf("expected synthetic summary right after system, got %q", out[1].Content)
	}
}

func TestMaybe_PreservesToolCallPair(t *testing.T) {
	msgs := []Message{
		mkMsg("user", strings.Repeat("a", 100)),
		mkMsg("assistant", strings.Repeat("b", 100)),
		mkMsg("user", strings.Repeat("c", 100)),
		mkMsg("user", strings.Repeat("d", 100)),
		mkMsg("user", strings.Repeat("e", 100)),
		{Role: "assistant", Content: "calling tool", HasToolCalls: true},
		{Role: "tool", Content: "tool result", ToolCallID: "call-1"},
		mkMsg("assistant", "done"),
	}
	res, err := Maybe(context.Background(),
		Policy{TriggerFraction: 0.3, KeepRecent: 2, MinReplacedMessages: 2},
		&State{}, msgs, 1000, charCount, okCaller("summary"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected compaction; result=%+v", res)
	}
	out := applySplice(msgs, res)
	hasToolCall := false
	hasToolResult := false
	for _, m := range out {
		if m.HasToolCalls {
			hasToolCall = true
		}
		if m.ToolCallID != "" {
			hasToolResult = true
		}
	}
	if !hasToolCall || !hasToolResult {
		t.Fatalf("tool-call pair severed: %+v", out)
	}
}

func TestMaybe_CircuitBreakerAfterFailures(t *testing.T) {
	st := &State{}
	msgs := makeBigHistory(40, 50)
	p := Policy{TriggerFraction: 0.1, KeepRecent: 2, MinReplacedMessages: 2, MaxFailures: 2}
	for i := 0; i < 5; i++ {
		_, _ = Maybe(context.Background(), p, st, msgs, 1000, charCount, failCaller(fmt.Errorf("boom")))
	}
	_, _, disabled, _ := st.Snapshot()
	if !disabled {
		t.Fatalf("expected disabled after failures")
	}
	res, err := Maybe(context.Background(), p, st, msgs, 1000, charCount, okCaller("summary"))
	if err != nil || res.Compacted {
		t.Fatalf("disabled state should no-op: err=%v res=%+v", err, res)
	}
}

func TestMaybe_EmptySummaryCountsAsFailure(t *testing.T) {
	st := &State{}
	msgs := makeBigHistory(40, 50)
	p := Policy{TriggerFraction: 0.1, KeepRecent: 2, MinReplacedMessages: 2, MaxFailures: 1}
	_, err := Maybe(context.Background(), p, st, msgs, 1000, charCount, okCaller(""))
	if err == nil {
		t.Fatalf("expected error on empty summary")
	}
	_, _, disabled, _ := st.Snapshot()
	if !disabled {
		t.Fatalf("expected disabled after MaxFailures=1")
	}
}

func TestMaybe_NoTokenLimitNoOp(t *testing.T) {
	msgs := makeBigHistory(40, 50)
	res, err := Maybe(context.Background(), Policy{}, &State{}, msgs, 0, charCount, okCaller("s"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Compacted {
		t.Fatalf("must not compact without a token limit")
	}
}

func TestMaybe_CounterFailureFallsBackToApprox(t *testing.T) {
	msgs := makeBigHistory(40, 50)
	st := &State{}
	failingCount := func(_ context.Context, _, _ string) (int, error) {
		return 0, fmt.Errorf("counter broke")
	}
	res, err := Maybe(context.Background(),
		Policy{TriggerFraction: 0.1, KeepRecent: 2, MinReplacedMessages: 2},
		st, msgs, 1000, failingCount, okCaller("summary"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("approx fallback should still trigger compaction: %+v", res)
	}
}

func makeBigHistory(n, sz int) []Message {
	out := make([]Message, n)
	for i := range out {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out[i] = Message{Role: role, Content: strings.Repeat("a", sz)}
	}
	return out
}
