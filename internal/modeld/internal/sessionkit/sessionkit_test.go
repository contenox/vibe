package sessionkit

import (
	"context"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/residency"
)

// TestSendReturnsFalseWhenCanceledAndFull pins the cancel-safe contract Decode
// loops rely on: a Send on a full channel must observe ctx cancellation and
// report false rather than block forever. (Relocated from the OpenVINO adapter,
// which owned a private copy of this helper.)
func TestSendReturnsFalseWhenCanceledAndFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan int, 1)
	ch <- 1 // fill it
	cancel()

	done := make(chan bool, 1)
	go func() { done <- Send(ctx, ch, 2) }()

	select {
	case sent := <-done:
		if sent {
			t.Fatal("Send reported a send on a full, canceled channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Send blocked on a full channel after cancellation")
	}
}

func TestSendDeliversWhenSlotFree(t *testing.T) {
	ch := make(chan int, 1)
	if !Send(context.Background(), ch, 7) {
		t.Fatal("Send reported false with a free slot")
	}
	if got := <-ch; got != 7 {
		t.Fatalf("Send delivered %d, want 7", got)
	}
}

func TestTrySendDropsWhenFull(t *testing.T) {
	ch := make(chan int, 1)
	ch <- 1
	TrySend(ch, 2) // must not block or panic
	if got := <-ch; got != 1 {
		t.Fatalf("TrySend overwrote the buffered value: got %d, want 1", got)
	}
}

func TestCommonPrefixLen(t *testing.T) {
	cases := []struct {
		a, b []int
		want int
	}{
		{nil, nil, 0},
		{[]int{1, 2, 3}, nil, 0},
		{[]int{1, 2, 3}, []int{1, 2, 9}, 2},
		{[]int{1, 2}, []int{1, 2, 3}, 2},
		{[]int{1, 2, 3}, []int{1, 2, 3}, 3},
		{[]int{9}, []int{1, 2}, 0},
	}
	for _, c := range cases {
		if got := CommonPrefixLen(c.a, c.b); got != c.want {
			t.Errorf("CommonPrefixLen(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResidencyReportMapsPlanAndCapabilities(t *testing.T) {
	plan := residency.Plan{
		BudgetTokens:    100,
		TotalTokens:     120,
		HotTokens:       100,
		ProtectedTokens: 40,
		OverBudget:      true,
		KeepHot:         make([]residency.Block, 2),
		EvictCold:       make([]residency.Block, 1),
		Diagnostics:     []string{"over budget"},
	}
	rep := ResidencyReport(plan, "boom", residency.Capabilities{
		RemoveTail:                   true,
		PositionShift:                true,
		SparseAttention:              true,
		SlidingWindowAttentionTokens: 4096,
	})
	if rep == nil {
		t.Fatal("ResidencyReport returned nil for a non-empty plan")
	}
	if rep.BudgetTokens != 100 || rep.TotalTokens != 120 || rep.HotTokens != 100 || rep.ColdTokens != 20 {
		t.Fatalf("token totals = %+v", rep)
	}
	if rep.ProtectedTokens != 40 || !rep.OverBudget || rep.HotBlocks != 2 || rep.ColdBlocks != 1 {
		t.Fatalf("plan shape = %+v", rep)
	}
	if !rep.Capabilities.RemoveTail || !rep.Capabilities.PositionShift || rep.Capabilities.RemoveMiddle {
		t.Fatalf("capabilities = %+v", rep.Capabilities)
	}
	if !rep.Capabilities.SparseAttention || rep.Capabilities.SlidingWindowAttentionTokens != 4096 {
		t.Fatalf("sparse capabilities = %+v", rep.Capabilities)
	}
	if rep.Error != "boom" || len(rep.Diagnostics) != 1 {
		t.Fatalf("error/diagnostics = %q / %v", rep.Error, rep.Diagnostics)
	}
}

func TestResidencyReportNilWhenEmpty(t *testing.T) {
	if rep := ResidencyReport(residency.Plan{}, "", residency.Capabilities{}); rep != nil {
		t.Fatalf("expected nil report for an empty plan, got %+v", rep)
	}
}

func TestChatRole(t *testing.T) {
	for _, kind := range []string{"system", "user", "assistant", "tool"} {
		if got := ChatRole(kind); got != kind {
			t.Errorf("ChatRole(%q) = %q, want %q", kind, got, kind)
		}
	}
	for _, kind := range []string{"", "bos", "control", "generation"} {
		if got := ChatRole(kind); got != "" {
			t.Errorf("ChatRole(%q) = %q, want empty (control segment)", kind, got)
		}
	}
}
