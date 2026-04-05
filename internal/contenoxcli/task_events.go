package contenoxcli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/taskengine"
)

type cliTaskEventRenderOptions struct {
	Trace        bool
	ShowThinking bool
}

type cliTaskEventRenderer struct {
	w              io.Writer
	trace          bool
	showThinking   bool
	lastTaskID     string
	contentActive  bool
	thinkingActive bool
}

func startCLITaskEventStream(
	ctx context.Context,
	engine *Engine,
	errW io.Writer,
	opts cliTaskEventRenderOptions,
) func() {
	requestID := requestIDFromContext(ctx)
	if requestID == "" || engine == nil {
		return func() {}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	eventCh := make(chan taskengine.TaskEvent, 32)
	sub, err := engine.WatchTaskEvents(streamCtx, requestID, eventCh)
	if err != nil {
		return func() {}
	}

	renderer := &cliTaskEventRenderer{
		w:            errW,
		trace:        opts.Trace,
		showThinking: opts.ShowThinking,
	}

	var once sync.Once
	go func() {
		for {
			select {
			case <-streamCtx.Done():
				return
			case event, ok := <-eventCh:
				if !ok {
					return
				}
				renderer.Render(event)
			}
		}
	}()

	return func() {
		once.Do(func() {
			cancel()
			_ = sub.Unsubscribe()
			renderer.Close()
		})
	}
}

func requestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	requestID, _ := ctx.Value(libtracker.ContextKeyRequestID).(string)
	return strings.TrimSpace(requestID)
}

func (r *cliTaskEventRenderer) Render(event taskengine.TaskEvent) {
	if r == nil || r.w == nil {
		return
	}
	if event.TaskID != "" && event.TaskID != r.lastTaskID {
		r.finishChunkLine()
		r.lastTaskID = event.TaskID
	}

	switch event.Kind {
	case taskengine.TaskEventChainStarted:
		if r.trace {
			fmt.Fprintf(r.w, "[taskengine] started %s\n", event.ChainID)
		}
	case taskengine.TaskEventStepStarted:
		r.finishChunkLine()
		if r.trace {
			fmt.Fprintf(r.w, "[step:%s] %s started\n", event.TaskID, event.TaskHandler)
		}
	case taskengine.TaskEventStepChunk:
		if event.Thinking != "" && r.showThinking {
			if r.contentActive {
				fmt.Fprintln(r.w)
				r.contentActive = false
			}
			if !r.thinkingActive {
				fmt.Fprint(r.w, "thinking: ")
				r.thinkingActive = true
			}
			fmt.Fprint(r.w, event.Thinking)
		}
		if event.Content != "" {
			if r.thinkingActive {
				fmt.Fprintln(r.w)
				r.thinkingActive = false
			}
			fmt.Fprint(r.w, event.Content)
			r.contentActive = true
		}
	case taskengine.TaskEventStepCompleted:
		r.finishChunkLine()
		if r.trace {
			fmt.Fprintf(r.w, "[step:%s] completed\n", event.TaskID)
		}
	case taskengine.TaskEventStepFailed:
		r.finishChunkLine()
		if r.trace {
			fmt.Fprintf(r.w, "[step:%s] failed: %s\n", event.TaskID, event.Error)
		}
	case taskengine.TaskEventChainCompleted:
		r.finishChunkLine()
		if r.trace {
			fmt.Fprintf(r.w, "[taskengine] completed %s\n", event.ChainID)
		}
	case taskengine.TaskEventChainFailed:
		r.finishChunkLine()
		if r.trace {
			fmt.Fprintf(r.w, "[taskengine] failed %s: %s\n", event.ChainID, event.Error)
		}
	}
}

func (r *cliTaskEventRenderer) Close() {
	if r == nil {
		return
	}
	r.finishChunkLine()
}

func (r *cliTaskEventRenderer) finishChunkLine() {
	if r == nil {
		return
	}
	if r.contentActive || r.thinkingActive {
		fmt.Fprintln(r.w)
	}
	r.contentActive = false
	r.thinkingActive = false
}
