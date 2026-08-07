package libroutine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/libroutine"
	"github.com/contenox/contenox/libtracker"
)

func TestUnit_Job_RunExecutesOperation(t *testing.T) {
	defer quiet()()
	var ran int32
	job := &libroutine.Job{
		Name: "root",
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		},
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Name != "root" || res.Skipped || res.Err != nil || res.Failed() {
		t.Fatalf("unexpected result: %+v", res)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("expected operation to run once, got %d", ran)
	}
}

func TestUnit_Job_FalseConditionSkipsOperation(t *testing.T) {
	defer quiet()()
	var ran int32
	job := &libroutine.Job{
		Name:      "root",
		Condition: func(ctx context.Context) (bool, error) { return false, nil },
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		},
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped || res.Failed() {
		t.Fatalf("expected skipped result, got %+v", res)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatalf("expected operation not to run, got %d calls", ran)
	}
}

func TestUnit_Job_ConditionErrorFailsWithoutRunningOperation(t *testing.T) {
	defer quiet()()
	var ran int32
	condErr := errors.New("condition boom")
	job := &libroutine.Job{
		Name:      "root",
		Condition: func(ctx context.Context) (bool, error) { return false, condErr },
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&ran, 1)
			return nil
		},
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected Run error: %v", err) // Run itself succeeds; the *job* failed
	}
	if !errors.Is(res.Err, condErr) || !res.Failed() {
		t.Fatalf("expected condition error in result, got %+v", res)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatalf("expected operation not to run, got %d calls", ran)
	}
}

func TestUnit_Job_OperationErrorStopsChain(t *testing.T) {
	defer quiet()()
	var nextRan int32
	opErr := errors.New("operation boom")
	next := &libroutine.Job{
		Name: "next",
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&nextRan, 1)
			return nil
		},
	}
	job := &libroutine.Job{
		Name:      "root",
		Operation: func(ctx context.Context) error { return opErr },
		Next:      next,
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(res.Err, opErr) {
		t.Fatalf("expected operation error, got %+v", res)
	}
	if res.Next != nil {
		t.Fatalf("expected chain to stop, got Next=%+v", res.Next)
	}
	if atomic.LoadInt32(&nextRan) != 0 {
		t.Fatalf("expected next job not to run, got %d calls", nextRan)
	}
}

func TestUnit_Job_ChainRunsNextOnSuccess(t *testing.T) {
	defer quiet()()
	var order []string
	var mu sync.Mutex
	record := func(name string) libroutine.Operation {
		return func(ctx context.Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	tail := &libroutine.Job{Name: "tail", Operation: record("tail")}
	mid := &libroutine.Job{Name: "mid", Operation: record("mid"), Next: tail}
	head := &libroutine.Job{Name: "head", Operation: record("head"), Next: mid}

	r := libroutine.NewRunner(head, 3, time.Second)
	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Failed() || res.Name != "head" || res.Next.Name != "mid" || res.Next.Next.Name != "tail" || res.Next.Next.Next != nil {
		t.Fatalf("unexpected chain result: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"head", "mid", "tail"}
	if len(order) != len(want) {
		t.Fatalf("expected order %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, order)
		}
	}
}

func TestUnit_Runner_RunReturnsErrAlreadyRunning(t *testing.T) {
	defer quiet()()
	release := make(chan struct{})
	entered := make(chan struct{})
	job := &libroutine.Job{
		Name: "slow",
		Operation: func(ctx context.Context) error {
			close(entered)
			<-release
			return nil
		},
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	go r.Run(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first run did not start in time")
	}

	_, err := r.Run(t.Context())
	if !errors.Is(err, libroutine.ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
	if !r.Running() {
		t.Fatal("expected Running to be true")
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.Running() {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Running() {
		t.Fatal("expected Running to become false")
	}
}

func TestUnit_Runner_TriggerDropsOverlappingRuns(t *testing.T) {
	defer quiet()()
	var starts int32
	release := make(chan struct{})
	job := &libroutine.Job{
		Name: "slow",
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&starts, 1)
			<-release
			return nil
		},
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	r.Trigger(context.Background())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !r.Running() {
		time.Sleep(5 * time.Millisecond)
	}

	// These should all be dropped since the first trigger is still in flight.
	r.Trigger(context.Background())
	r.Trigger(context.Background())
	time.Sleep(50 * time.Millisecond)

	close(release)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && r.Running() {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("expected exactly 1 start, got %d", got)
	}
}

func TestUnit_Runner_CircuitOpensAfterRepeatedFailures(t *testing.T) {
	defer quiet()()
	var attempts int32
	failErr := errors.New("boom")
	job := &libroutine.Job{
		Name: "flaky",
		Operation: func(ctx context.Context) error {
			atomic.AddInt32(&attempts, 1)
			return failErr
		},
	}
	r := libroutine.NewRunner(job, 1, time.Minute) // trips open after a single failure

	res1, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("unexpected Run error on first attempt: %v", err)
	}
	if !res1.Failed() {
		t.Fatalf("expected first run to fail, got %+v", res1)
	}

	// The circuit is now open: a second Run must not invoke the operation
	// again — this is exactly the "flaky job hammered forever" case a
	// bare single-flight guard (with no circuit breaker) would not catch.
	_, err = r.Run(t.Context())
	if !errors.Is(err, libroutine.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected operation to have run exactly once, got %d", got)
	}
}

func TestUnit_Runner_ResultHookReceivesEveryRun(t *testing.T) {
	defer quiet()()
	var got []*libroutine.RunResult
	var mu sync.Mutex
	job := &libroutine.Job{
		Name:      "root",
		Operation: func(ctx context.Context) error { return nil },
	}
	r := libroutine.NewRunner(job, 3, time.Second, libroutine.WithResultHook(func(res *libroutine.RunResult) {
		mu.Lock()
		got = append(got, res)
		mu.Unlock()
	}))

	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Name != "root" {
		t.Fatalf("expected one hooked result named root, got %+v", got)
	}
}

// fakeTracker records ActivityTracker calls for assertion.
type fakeTracker struct {
	mu       sync.Mutex
	starts   []string
	errs     []error
	changes  []string
	endCalls int
}

func (f *fakeTracker) Start(ctx context.Context, operation, subject string, kvArgs ...any) (func(error), func(string, any), func()) {
	f.mu.Lock()
	f.starts = append(f.starts, operation+":"+subject)
	f.mu.Unlock()
	return func(err error) {
			f.mu.Lock()
			f.errs = append(f.errs, err)
			f.mu.Unlock()
		}, func(id string, data any) {
			f.mu.Lock()
			f.changes = append(f.changes, id)
			f.mu.Unlock()
		}, func() {
			f.mu.Lock()
			f.endCalls++
			f.mu.Unlock()
		}
}

var _ libtracker.ActivityTracker = (*fakeTracker)(nil)

func TestUnit_Runner_TrackerObservesSuccessAndFailure(t *testing.T) {
	defer quiet()()
	tracker := &fakeTracker{}

	okJob := &libroutine.Job{Name: "ok", Operation: func(ctx context.Context) error { return nil }}
	okRunner := libroutine.NewRunner(okJob, 3, time.Second, libroutine.WithTracker(tracker))
	if _, err := okRunner.Run(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	failErr := errors.New("boom")
	failJob := &libroutine.Job{Name: "fail", Operation: func(ctx context.Context) error { return failErr }}
	failRunner := libroutine.NewRunner(failJob, 3, time.Second, libroutine.WithTracker(tracker))
	if _, err := failRunner.Run(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.starts) != 2 || len(tracker.changes) != 1 || len(tracker.errs) != 1 || tracker.endCalls != 2 {
		t.Fatalf("unexpected tracker state: %+v", tracker)
	}
	if tracker.changes[0] != "ok" {
		t.Fatalf("expected reportChange for the ok job, got %v", tracker.changes)
	}
	if !errors.Is(tracker.errs[0], failErr) {
		t.Fatalf("expected reportErr with the operation error, got %v", tracker.errs[0])
	}
}

func TestUnit_Schedule_EveryFiresRepeatedly(t *testing.T) {
	defer quiet()()
	var count int32
	job := &libroutine.Job{
		Name:      "ticker",
		Operation: func(ctx context.Context) error { atomic.AddInt32(&count, 1); return nil },
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartSchedule(ctx, libroutine.Every(20*time.Millisecond))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&count) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&count); got < 3 {
		t.Fatalf("expected at least 3 ticks, got %d", got)
	}
}

func TestUnit_Schedule_StopsOnContextCancel(t *testing.T) {
	defer quiet()()
	var count int32
	job := &libroutine.Job{
		Name:      "ticker",
		Operation: func(ctx context.Context) error { atomic.AddInt32(&count, 1); return nil },
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	r.StartSchedule(ctx, libroutine.Every(15*time.Millisecond))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&count) < 1 {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	after := atomic.LoadInt32(&count)
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&count); got > after+1 {
		t.Fatalf("expected schedule to stop after cancel, went from %d to %d", after, got)
	}
}

func TestUnit_SubscribeMessenger_TriggersOnPublish(t *testing.T) {
	defer quiet()()
	var ran int32
	job := &libroutine.Job{
		Operation: func(ctx context.Context) error { atomic.AddInt32(&ran, 1); return nil },
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	bus := libbus.NewInMem()
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := r.SubscribeMessenger(ctx, bus, "process.myproc.running"); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	if err := bus.Publish(ctx, "process.myproc.running", []byte("1")); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&ran) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("expected job to run once, got %d", ran)
	}
}

func TestUnit_SubscribeMessenger_IgnoresOtherSubjects(t *testing.T) {
	defer quiet()()
	var ran int32
	job := &libroutine.Job{
		Operation: func(ctx context.Context) error { atomic.AddInt32(&ran, 1); return nil },
	}
	r := libroutine.NewRunner(job, 3, time.Second)

	bus := libbus.NewInMem()
	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := r.SubscribeMessenger(ctx, bus, "process.myproc.running"); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	if err := bus.Publish(ctx, "process.myproc.stopped", []byte("1")); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatalf("expected job not to run for an unsubscribed subject, got %d", ran)
	}
}
