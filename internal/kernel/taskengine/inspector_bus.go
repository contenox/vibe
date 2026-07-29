package taskengine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libtracker"
)

func StateSubject(reqID string) string {
	return "state." + reqID
}

type BusInspector struct {
	inner   Inspector
	bus     libbus.Messenger
	tracker libtracker.ActivityTracker
}

func NewBusInspector(inner Inspector, bus libbus.Messenger, tracker libtracker.ActivityTracker) *BusInspector {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &BusInspector{inner: inner, bus: bus, tracker: tracker}
}

func (i *BusInspector) Start(ctx context.Context) StackTrace {
	inner := i.inner.Start(ctx)
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return inner
	}
	return &busStackTrace{
		inner:   inner,
		bus:     i.bus,
		subject: StateSubject(reqID),
		ctx:     ctx,
		tracker: i.tracker,
	}
}

type busStackTrace struct {
	inner   StackTrace
	bus     libbus.Messenger
	subject string
	ctx     context.Context
	tracker libtracker.ActivityTracker
}

func (s *busStackTrace) RecordStep(step CapturedStateUnit) {
	s.inner.RecordStep(step)
	published := sanitizeCapturedStateForPersistence(step)

	reportErr, _, end := s.tracker.Start(s.ctx, "publish_step", "state_bus",
		"subject", s.subject, "task_id", step.TaskID)
	defer end()

	data, err := json.Marshal(published)
	if err != nil {
		reportErr(err)
		return
	}

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()

	if err := s.bus.Publish(pubCtx, s.subject, data); err != nil {
		reportErr(err)
	}
}

func (s *busStackTrace) GetExecutionHistory() []CapturedStateUnit {
	return s.inner.GetExecutionHistory()
}
