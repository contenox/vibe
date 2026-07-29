package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	libkv "github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/libtracker"
)

const (
	kvStateRequestsSet = "state:requests"
	kvStatePrefix      = "state:"
	kvStateMaxEntries  = 1000
)

type KVInspector struct {
	inner   Inspector
	kv      libkv.KVManager
	tracker libtracker.ActivityTracker
}

func NewKVInspector(inner Inspector, kv libkv.KVManager, tracker libtracker.ActivityTracker) *KVInspector {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	return &KVInspector{inner: inner, kv: kv, tracker: tracker}
}

func (i *KVInspector) Start(ctx context.Context) StackTrace {
	inner := i.inner.Start(ctx)
	reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string)
	if !ok || reqID == "" {
		return inner
	}

	reportErr, _, end := i.tracker.Start(ctx, "register_request", "state_kv", "request_id", reqID)
	defer end()

	op, err := i.kv.Executor(ctx)
	if err != nil {
		reportErr(err)
		return inner
	}
	reqIDJSON, err := json.Marshal(reqID)
	if err != nil {
		reportErr(err)
		return inner
	}
	if err := op.SetAdd(ctx, kvStateRequestsSet, reqIDJSON); err != nil {
		reportErr(err)
		return inner
	}

	return &kvStackTrace{
		inner:   inner,
		kv:      i.kv,
		reqID:   reqID,
		ctx:     ctx,
		tracker: i.tracker,
	}
}

func (i *KVInspector) GetExecutionStateByRequestID(ctx context.Context, reqID string) ([]CapturedStateUnit, error) {
	if reqID == "" {
		return nil, nil
	}
	reportErr, _, end := i.tracker.Start(ctx, "get_state", "state_kv", "request_id", reqID)
	defer end()

	op, err := i.kv.Executor(ctx)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	raw, err := op.ListRange(ctx, kvStatePrefix+reqID, 0, -1)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	out := make([]CapturedStateUnit, 0, len(raw))
	for idx, b := range raw {
		var u CapturedStateUnit
		if err := json.Unmarshal(b, &u); err != nil {
			reportErr(fmt.Errorf("unmarshal step %d: %w", idx, err))
			continue
		}
		out = append(out, u)
	}
	// ListPush prepends (LPUSH); reverse so callers get execution order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (i *KVInspector) GetStatefulRequests(ctx context.Context) ([]string, error) {
	reportErr, _, end := i.tracker.Start(ctx, "list_requests", "state_kv")
	defer end()

	op, err := i.kv.Executor(ctx)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	raw, err := op.SetMembers(ctx, kvStateRequestsSet)
	if err != nil {
		reportErr(err)
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, b := range raw {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			reportErr(err)
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

type kvStackTrace struct {
	inner   StackTrace
	kv      libkv.KVManager
	reqID   string
	ctx     context.Context
	tracker libtracker.ActivityTracker
}

func (s *kvStackTrace) RecordStep(step CapturedStateUnit) {
	s.inner.RecordStep(step)
	persisted := sanitizeCapturedStateForPersistence(step)

	reportErr, _, end := s.tracker.Start(s.ctx, "persist_step", "state_kv",
		"request_id", s.reqID, "task_id", step.TaskID)
	defer end()

	data, err := json.Marshal(persisted)
	if err != nil {
		reportErr(err)
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 10*time.Second)
	defer cancel()

	op, err := s.kv.Executor(writeCtx)
	if err != nil {
		reportErr(err)
		return
	}

	key := kvStatePrefix + s.reqID
	if err := op.ListPush(writeCtx, key, data); err != nil {
		reportErr(err)
		return
	}
	n, err := op.ListLength(writeCtx, key)
	if err != nil {
		reportErr(err)
		return
	}
	if n > kvStateMaxEntries {
		if err := op.ListTrim(writeCtx, key, 0, kvStateMaxEntries-1); err != nil {
			reportErr(err)
		}
	}
}

func (s *kvStackTrace) GetExecutionHistory() []CapturedStateUnit {
	return s.inner.GetExecutionHistory()
}
