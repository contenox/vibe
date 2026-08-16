package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
)

// traceDrainGrace is how long to wait after a chain returns before cancelling
// the trace subscription, so the bus poller can deliver a final step.
const traceDrainGrace = 500 * time.Millisecond

// startTraceStream subscribes to the per-request state bus subject and renders
// each captured step to w in real time, returning a stop function. It no-ops
// when --trace is off, the engine has no bus, or the subscription fails.
func startTraceStream(ctx context.Context, opts chatOpts, engine *Engine, w io.Writer) func() {
	if !opts.EffectiveTracing || engine == nil || engine.Bus == nil {
		return func() {}
	}
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return func() {}
	}

	subject := taskengine.StateSubject(reqID)

	tracker := engine.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, _, end := tracker.Start(ctx, "subscribe", "state_bus", "subject", subject)
	defer end()

	streamCtx, cancel := context.WithCancel(ctx)
	rawCh := make(chan []byte, 32)
	sub, err := engine.Bus.Stream(streamCtx, subject, rawCh)
	if err != nil {
		cancel()
		reportErr(err)
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		renderTraceUnits(streamCtx, rawCh, w)
		close(done)
	}()

	return func() {
		time.Sleep(traceDrainGrace)
		cancel()
		_ = sub.Unsubscribe()
		<-done
	}
}

func startThoughtStream(ctx context.Context, engine *Engine, w io.Writer, thinkLevel string) func() {
	if !shouldPrintThinking(thinkLevel) || engine == nil || engine.Bus == nil {
		return func() {}
	}
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return func() {}
	}
	subject := taskengine.TaskEventRequestSubject(reqID)

	tracker := engine.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, _, end := tracker.Start(ctx, "subscribe", "thinking_event_bus", "subject", subject)
	defer end()

	streamCtx, cancel := context.WithCancel(ctx)
	rawCh := make(chan []byte, 32)
	sub, err := engine.Bus.Stream(streamCtx, subject, rawCh)
	if err != nil {
		cancel()
		reportErr(err)
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		renderThinkingEvents(streamCtx, rawCh, w)
		close(done)
	}()

	return func() {
		time.Sleep(traceDrainGrace)
		cancel()
		_ = sub.Unsubscribe()
		<-done
	}
}

func startPrintStream(ctx context.Context, engine *Engine, w io.Writer) func() {
	if engine == nil || engine.Bus == nil {
		return func() {}
	}
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return func() {}
	}
	subject := taskengine.TaskEventRequestSubject(reqID)

	tracker := engine.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	reportErr, _, end := tracker.Start(ctx, "subscribe", "task_event_bus", "subject", subject)
	defer end()

	streamCtx, cancel := context.WithCancel(ctx)
	rawCh := make(chan []byte, 32)
	sub, err := engine.Bus.Stream(streamCtx, subject, rawCh)
	if err != nil {
		cancel()
		reportErr(err)
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		renderPrintEvents(streamCtx, rawCh, w)
		close(done)
	}()

	return func() {
		time.Sleep(traceDrainGrace)
		cancel()
		_ = sub.Unsubscribe()
		<-done
	}
}

func renderThinkingEvents(ctx context.Context, ch <-chan []byte, w io.Writer) {
	started := false
	for {
		select {
		case <-ctx.Done():
			if started {
				_, _ = fmt.Fprintln(w)
			}
			return
		case payload, ok := <-ch:
			if !ok {
				if started {
					_, _ = fmt.Fprintln(w)
				}
				return
			}
			var ev taskengine.TaskEvent
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			if ev.Kind != taskengine.TaskEventStepChunk || ev.Thinking == "" {
				continue
			}
			if !started {
				if _, err := fmt.Fprint(w, "\nReasoning:\n"); err != nil {
					return
				}
				started = true
			}
			if _, err := fmt.Fprint(w, ev.Thinking); err != nil {
				return
			}
		}
	}
}

func renderPrintEvents(ctx context.Context, ch <-chan []byte, w io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			var ev taskengine.TaskEvent
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			if ev.Kind == taskengine.TaskEventPrint && ev.Content != "" {
				if _, err := fmt.Fprintln(w, ev.Content); err != nil {
					return
				}
			}
		}
	}
}

func renderTraceUnits(ctx context.Context, ch <-chan []byte, w io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			var unit taskengine.CapturedStateUnit
			if err := json.Unmarshal(payload, &unit); err != nil {
				continue
			}
			if _, err := fmt.Fprintln(w, formatTraceUnit(unit)); err != nil {
				return
			}
		}
	}
}

func formatTraceUnit(u taskengine.CapturedStateUnit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[trace] task=%s handler=%s retry=%d dur=%s trans=%s",
		u.TaskID, u.TaskHandler, u.RetryIndex, u.Duration, u.Transition)
	if u.ModelName != "" {
		fmt.Fprintf(&b, " model=%s", u.ModelName)
	}
	if u.ProviderType != "" {
		fmt.Fprintf(&b, " provider=%s", u.ProviderType)
	}
	if len(u.ToolNames) > 0 {
		fmt.Fprintf(&b, " tools=%s", strings.Join(u.ToolNames, ","))
	}
	if u.TokenUsage != nil {
		fmt.Fprintf(&b, " tokens=%d+%d=%d", u.TokenUsage.Prompt, u.TokenUsage.Completion, u.TokenUsage.Total)
	}
	switch {
	case u.TimedOut:
		b.WriteString(" TIMED-OUT")
	case u.Cancelled:
		b.WriteString(" CANCELLED")
	}
	if u.Error.Error != "" {
		fmt.Fprintf(&b, " ERROR: %s", u.Error.Error)
	}
	return b.String()
}
