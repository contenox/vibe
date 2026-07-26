package contenoxcli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_FormatTraceUnit_Minimal(t *testing.T) {
	u := taskengine.CapturedStateUnit{
		TaskID:      "respond",
		TaskHandler: "chat_completion",
		Duration:    250 * time.Millisecond,
		Transition:  "ok",
	}
	got := formatTraceUnit(u)
	assert.Contains(t, got, "task=respond")
	assert.Contains(t, got, "handler=chat_completion")
	assert.Contains(t, got, "retry=0")
	assert.Contains(t, got, "trans=ok")
	assert.NotContains(t, got, "ERROR")
	assert.NotContains(t, got, "TIMED-OUT")
	assert.NotContains(t, got, "CANCELLED")
}

func TestUnit_FormatTraceUnit_LLMStepWithUsage(t *testing.T) {
	u := taskengine.CapturedStateUnit{
		TaskID:       "respond",
		TaskHandler:  "chat_completion",
		Duration:     100 * time.Millisecond,
		Transition:   "ok",
		ModelName:    "qwen2.5:7b",
		ProviderType: "ollama",
		TokenUsage:   &taskengine.TokenUsage{Prompt: 12, Completion: 34, Total: 46},
	}
	got := formatTraceUnit(u)
	assert.Contains(t, got, "model=qwen2.5:7b")
	assert.Contains(t, got, "provider=ollama")
	assert.Contains(t, got, "tokens=12+34=46")
}

func TestUnit_FormatTraceUnit_ToolCallStep(t *testing.T) {
	u := taskengine.CapturedStateUnit{
		TaskID:      "exec",
		TaskHandler: "execute_tool_calls",
		ToolNames:   []string{"local_fs.read_file", "webtools.get"},
	}
	got := formatTraceUnit(u)
	assert.Contains(t, got, "tools=local_fs.read_file,webtools.get")
}

func TestUnit_FormatTraceUnit_TimedOut(t *testing.T) {
	u := taskengine.CapturedStateUnit{
		TaskID:      "respond",
		TaskHandler: "chat_completion",
		TimedOut:    true,
		Error:       taskengine.ErrorResponse{Error: "context deadline exceeded"},
	}
	got := formatTraceUnit(u)
	assert.Contains(t, got, "TIMED-OUT")
	assert.Contains(t, got, "ERROR: context deadline exceeded")
}

func TestUnit_FormatTraceUnit_Cancelled(t *testing.T) {
	u := taskengine.CapturedStateUnit{
		TaskID:      "respond",
		TaskHandler: "chat_completion",
		Cancelled:   true,
	}
	got := formatTraceUnit(u)
	assert.Contains(t, got, "CANCELLED")
}

// syncBuffer guards a bytes.Buffer so the renderer goroutine's writes and the
// test's Eventually reads don't race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestUnit_RenderTraceUnits_StreamsAndExitsOnContext(t *testing.T) {
	var buf syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan []byte, 4)
	done := make(chan struct{})
	go func() {
		renderTraceUnits(ctx, ch, &buf)
		close(done)
	}()

	for i := 0; i < 3; i++ {
		u := taskengine.CapturedStateUnit{TaskID: "t", TaskHandler: "h", RetryIndex: i}
		data, err := json.Marshal(u)
		require.NoError(t, err)
		ch <- data
	}

	require.Eventually(t, func() bool {
		return strings.Count(buf.String(), "\n") == 3
	}, time.Second, 10*time.Millisecond, "renderer didn't drain three units")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renderer did not exit on context cancel")
	}

	out := buf.String()
	assert.Contains(t, out, "retry=0")
	assert.Contains(t, out, "retry=1")
	assert.Contains(t, out, "retry=2")
}

func TestUnit_RenderTraceUnits_ExitsWhenChannelClosed(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	ch := make(chan []byte, 1)

	u := taskengine.CapturedStateUnit{TaskID: "t", TaskHandler: "h"}
	data, _ := json.Marshal(u)
	ch <- data
	close(ch)

	done := make(chan struct{})
	go func() {
		renderTraceUnits(ctx, ch, &buf)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renderer did not exit on channel close")
	}

	assert.Contains(t, buf.String(), "task=t")
}

func TestUnit_RenderTraceUnits_SkipsBadJSON(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	ch := make(chan []byte, 2)
	ch <- []byte("not json")
	good := taskengine.CapturedStateUnit{TaskID: "good", TaskHandler: "h"}
	data, _ := json.Marshal(good)
	ch <- data
	close(ch)

	done := make(chan struct{})
	go func() {
		renderTraceUnits(ctx, ch, &buf)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renderer did not exit")
	}

	out := buf.String()
	assert.Contains(t, out, "task=good")
	assert.Equal(t, 1, strings.Count(out, "\n"))
}

func TestUnit_RenderThinkingEvents_OnlyWritesThinkingChunks(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	ch := make(chan []byte, 4)
	thinking, _ := json.Marshal(taskengine.TaskEvent{Kind: taskengine.TaskEventStepChunk, Thinking: "step 1"})
	content, _ := json.Marshal(taskengine.TaskEvent{Kind: taskengine.TaskEventStepChunk, Content: "visible answer"})
	completed, _ := json.Marshal(taskengine.TaskEvent{Kind: taskengine.TaskEventStepCompleted, Thinking: "ignored"})
	ch <- []byte("not json")
	ch <- content
	ch <- thinking
	ch <- completed
	close(ch)

	renderThinkingEvents(ctx, ch, &buf)
	out := buf.String()
	assert.Contains(t, out, "Reasoning:\nstep 1")
	assert.NotContains(t, out, "visible answer")
	assert.NotContains(t, out, "ignored")
}
