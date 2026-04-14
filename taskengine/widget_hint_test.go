package taskengine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestWidgetHintSink_AppendDrain(t *testing.T) {
	s := &WidgetHintSink{}
	s.Append(WidgetHint{Kind: "file_view", Payload: json.RawMessage(`{"a":1}`)})
	s.Append(WidgetHint{Kind: "terminal_excerpt"})
	got := s.Drain()
	if len(got) != 2 {
		t.Fatalf("drain returned %d hints, want 2", len(got))
	}
	if got[0].Kind != "file_view" || got[1].Kind != "terminal_excerpt" {
		t.Fatalf("unexpected order: %+v", got)
	}
	// Drain clears.
	if again := s.Drain(); len(again) != 0 {
		t.Fatalf("second drain returned %d, want 0", len(again))
	}
}

func TestWidgetHintSink_NilSafe(t *testing.T) {
	var s *WidgetHintSink
	s.Append(WidgetHint{Kind: "x"})
	if got := s.Drain(); got != nil {
		t.Fatalf("nil sink Drain should return nil")
	}
	if got := s.Snapshot(); got != nil {
		t.Fatalf("nil sink Snapshot should return nil")
	}
}

func TestAppendWidgetHint_NoSinkIsNoOp(t *testing.T) {
	// No sink in context — must not panic, must not cost anything observable.
	AppendWidgetHint(context.Background(), WidgetHint{Kind: "x"})
	AppendWidgetHintTyped(context.Background(), "x", map[string]any{"a": 1})
}

func TestAppendWidgetHint_WithSink(t *testing.T) {
	s := &WidgetHintSink{}
	ctx := WithWidgetHintSink(context.Background(), s)
	AppendWidgetHint(ctx, WidgetHint{Kind: "file_view"})
	AppendWidgetHintTyped(ctx, "terminal_excerpt", map[string]any{"output": "hi"})
	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d hints, want 2", len(got))
	}
	if got[1].Kind != "terminal_excerpt" {
		t.Fatalf("kind = %q", got[1].Kind)
	}
	var p struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(got[1].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Output != "hi" {
		t.Fatalf("decoded output = %q", p.Output)
	}
}

func TestAppendWidgetHint_ConcurrentAppend(t *testing.T) {
	s := &WidgetHintSink{}
	ctx := WithWidgetHintSink(context.Background(), s)
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			AppendWidgetHint(ctx, WidgetHint{Kind: "x"})
		}()
	}
	wg.Wait()
	if got := len(s.Drain()); got != N {
		t.Fatalf("got %d hints, want %d", got, N)
	}
}

func TestWithWidgetHintSink_NilNoop(t *testing.T) {
	ctx := WithWidgetHintSink(context.Background(), nil)
	// Should round-trip the original ctx, so AppendWidgetHint is a no-op.
	AppendWidgetHint(ctx, WidgetHint{Kind: "x"})
}
