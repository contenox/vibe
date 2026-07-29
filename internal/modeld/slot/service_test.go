package slot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/modelstore"
	"github.com/contenox/contenox/internal/transport"
)

type fakeBackend struct {
	mu       sync.Mutex
	opened   []string
	closed   []string
	openErr  error
	sessions []*fakeSession
	// requiredBytes is returned as ModelInfo.RequiredBytes so tests can control
	// the footprint estimate the slot captures at load time.
	requiredBytes int64
	describes     []transport.OpenSessionRequest
	// embedErr, when set, is returned by Embed. embeds records the requests that
	// reached the backend.
	embedErr error
	embeds   []transport.EmbedRequest
}

func (b *fakeBackend) OpenSession(_ context.Context, req transport.OpenSessionRequest) (transport.Session, error) {
	if b.openErr != nil {
		return nil, b.openErr
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &fakeSession{name: req.ModelName, owner: b}
	b.opened = append(b.opened, req.ModelName)
	b.sessions = append(b.sessions, s)
	return s, nil
}

func (b *fakeBackend) Describe(_ context.Context, req transport.OpenSessionRequest) (transport.ModelInfo, error) {
	b.mu.Lock()
	b.describes = append(b.describes, req)
	required := b.requiredBytes
	b.mu.Unlock()
	eff := req.Config.NumCtx
	if eff <= 0 {
		eff = 1024
	}
	return transport.ModelInfo{ModelMaxContext: req.Config.NumCtx, EffectiveContext: eff, RequiredBytes: required}, nil
}

func (b *fakeBackend) lastDescribe() transport.OpenSessionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.describes) == 0 {
		return transport.OpenSessionRequest{}
	}
	return b.describes[len(b.describes)-1]
}

func (b *fakeBackend) Embed(_ context.Context, req transport.EmbedRequest) (transport.EmbedResult, error) {
	b.mu.Lock()
	b.embeds = append(b.embeds, req)
	err := b.embedErr
	b.mu.Unlock()
	if err != nil {
		return transport.EmbedResult{}, err
	}
	return transport.EmbedResult{Vector: []float32{1}}, nil
}

func (b *fakeBackend) recordClosed(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = append(b.closed, name)
}

type fakeSession struct {
	name      string
	owner     *fakeBackend
	closed    bool
	prefixErr error
	decodeCh  <-chan transport.StreamChunk
}

func (s *fakeSession) EnsurePrefix(context.Context, transport.PrefixInput) (transport.PrefixStatus, error) {
	if s.closed {
		return transport.PrefixStatus{}, transport.ErrSessionClosed
	}
	if s.prefixErr != nil {
		return transport.PrefixStatus{}, s.prefixErr
	}
	return transport.PrefixStatus{PrefixTokens: 1}, nil
}

func (s *fakeSession) PrefillSuffix(context.Context, transport.SuffixInput) (transport.SuffixStatus, error) {
	if s.closed {
		return transport.SuffixStatus{}, transport.ErrSessionClosed
	}
	return transport.SuffixStatus{SuffixTokens: 1}, nil
}

func (s *fakeSession) Decode(context.Context, transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	if s.closed {
		return nil, transport.ErrSessionClosed
	}
	if s.decodeCh != nil {
		return s.decodeCh, nil
	}
	ch := make(chan transport.StreamChunk)
	close(ch)
	return ch, nil
}

func (s *fakeSession) ExplainContext() transport.ContextReport {
	return transport.ContextReport{}
}

func (s *fakeSession) Snapshot(context.Context) (transport.SessionSnapshot, error) {
	if s.closed {
		return transport.SessionSnapshot{}, transport.ErrSessionClosed
	}
	return transport.SessionSnapshot{}, nil
}

func (s *fakeSession) Restore(context.Context, transport.SessionSnapshot) error {
	if s.closed {
		return transport.ErrSessionClosed
	}
	return nil
}

func (s *fakeSession) Close() error {
	s.closed = true
	if s.owner != nil {
		s.owner.recordClosed(s.name)
	}
	return nil
}

func req(name string) transport.OpenSessionRequest {
	return transport.OpenSessionRequest{
		ModelName: name,
		Type:      "llama",
		Digest:    "digest-" + name,
		Path:      "/models/" + name,
		Config:    transport.Config{NumCtx: 1024},
	}
}

func loadReq(name string) transport.LoadModelRequest {
	r := req(name)
	return transport.LoadModelRequest{
		ModelName: r.ModelName,
		Type:      r.Type,
		Digest:    r.Digest,
		Path:      r.Path,
		Config:    r.Config,
	}
}

func TestUnit_Slot_OpenSessionAutoLoadsAndCloseReleasesImplicitModel(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))

	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotReady || st.Active == nil || st.Active.ModelName != "a" {
		t.Fatalf("status after open = %+v", st)
	}
	if _, err := svc.OpenSession(ctx, req("b")); !errors.Is(err, transport.ErrModelBusy) {
		t.Fatalf("OpenSession different while held = %v, want ErrModelBusy", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st, _ = svc.Status(ctx)
	if st.State != transport.SlotEmpty || st.Active != nil {
		t.Fatalf("status after close = %+v, want empty", st)
	}
	if len(backend.sessions) != 1 || !backend.sessions[0].closed {
		t.Fatalf("implicit session was not closed on release")
	}
}

func TestUnit_Slot_SessionFatalEvictsActiveSlotAndReopens(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))

	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	backend.sessions[0].prefixErr = transport.ErrSessionFatal
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{}); !errors.Is(err, transport.ErrSessionFatal) {
		t.Fatalf("EnsurePrefix fatal = %v, want ErrSessionFatal", err)
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotFailed || st.Active != nil {
		t.Fatalf("status after fatal = %+v, want failed/no-active", st)
	}
	if !backend.sessions[0].closed {
		t.Fatal("fatal backend session was not closed")
	}

	next, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession after fatal: %v", err)
	}
	defer next.Close()
	if len(backend.sessions) != 2 {
		t.Fatalf("backend sessions = %d, want reopened session", len(backend.sessions))
	}
}

func TestUnit_Slot_ExplicitSwitchRequiresReleasedSessionAndInvalidatesOldHandle(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))

	activeA, err := svc.LoadModel(ctx, loadReq("a"))
	if err != nil {
		t.Fatalf("LoadModel a: %v", err)
	}
	sessA, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession a: %v", err)
	}
	if _, err := svc.LoadModel(ctx, loadReq("b")); !errors.Is(err, transport.ErrModelBusy) {
		t.Fatalf("LoadModel b while a held = %v, want ErrModelBusy", err)
	}
	if err := sessA.Close(); err != nil {
		t.Fatalf("Close a: %v", err)
	}
	activeB, err := svc.LoadModel(ctx, loadReq("b"))
	if err != nil {
		t.Fatalf("LoadModel b after release: %v", err)
	}
	if activeB.Generation <= activeA.Generation {
		t.Fatalf("generation did not advance: a=%d b=%d", activeA.Generation, activeB.Generation)
	}
	if _, err := sessA.EnsurePrefix(ctx, transport.PrefixInput{}); !errors.Is(err, transport.ErrSessionClosed) {
		t.Fatalf("old session after close/switch = %v, want ErrSessionClosed", err)
	}
	st, _ := svc.Status(ctx)
	if st.Active == nil || st.Active.ModelName != "b" {
		t.Fatalf("status after switch = %+v", st)
	}
	if len(backend.sessions) < 1 || !backend.sessions[0].closed {
		t.Fatalf("old backend session was not closed during switch")
	}
}

// A base model and the same base + a LoRA adapter are different variants: the
// slot must treat an adapter change as a model switch, never silently reuse the
// base's resident slot for a variant. This is the load-bearing cache-safety rule.
func TestUnit_Slot_AdapterChangeIsAModelSwitch(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))

	// Base model resident (explicit load keeps it with no open handle).
	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel base: %v", err)
	}
	// Reopening the SAME base identity reuses the resident slot.
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession base reuse: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close base: %v", err)
	}

	// Same base + an adapter is a distinct variant: opening it against the resident
	// base must require a switch, not reuse the base session.
	variant := req("a")
	variant.Adapters = []transport.AdapterSpec{{Name: "style", Path: "/adapters/style.gguf", Digest: "adapter-1", Scale: 1}}
	if _, err := svc.OpenSession(ctx, variant); !errors.Is(err, transport.ErrModelSwitchRequired) {
		t.Fatalf("OpenSession base+adapter = %v, want ErrModelSwitchRequired", err)
	}

	// Once loaded, the active model reports the adapter identity.
	lr := loadReq("a")
	lr.Adapters = variant.Adapters
	active, err := svc.LoadModel(ctx, lr)
	if err != nil {
		t.Fatalf("LoadModel variant: %v", err)
	}
	if len(active.Adapters) != 1 || active.Adapters[0].Digest != "adapter-1" {
		t.Fatalf("ActiveModel adapters = %+v, want adapter-1", active.Adapters)
	}
}

func TestUnit_Slot_UnloadExplicitModel(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeBackend{}, WithBackend("llama"))

	active, err := svc.LoadModel(ctx, loadReq("a"))
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if err := svc.UnloadModel(ctx, transport.UnloadModelRequest{ExpectedGeneration: active.Generation + 1}); !errors.Is(err, transport.ErrSlotGenerationStale) {
		t.Fatalf("Unload stale generation = %v, want ErrSlotGenerationStale", err)
	}
	if err := svc.UnloadModel(ctx, transport.UnloadModelRequest{ExpectedGeneration: active.Generation}); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotEmpty || st.Active != nil {
		t.Fatalf("status after unload = %+v", st)
	}
}

func TestUnit_Slot_LoadFailureLeavesFailedWithoutActiveModel(t *testing.T) {
	ctx := context.Background()
	svc := New(&fakeBackend{openErr: transport.ErrContextOverflow}, WithBackend("llama"))

	_, err := svc.LoadModel(ctx, loadReq("a"))
	if !errors.Is(err, transport.ErrModelLoadFailed) || !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("LoadModel error = %v, want load failed and context overflow", err)
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotFailed || st.Active != nil || st.LastError == "" {
		t.Fatalf("status after failed load = %+v", st)
	}
}

func TestUnit_Slot_CanceledLoadWhileBusyDoesNotRunLater(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))
	active, err := svc.LoadModel(ctx, loadReq("a"))
	if err != nil {
		t.Fatalf("LoadModel a: %v", err)
	}
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession a: %v", err)
	}

	block := make(chan transport.StreamChunk)
	backend.sessions[0].decodeCh = block
	decodeCtx, cancelDecode := context.WithCancel(ctx)
	ch, err := sess.Decode(decodeCtx, transport.DecodeConfig{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	defer func() {
		cancelDecode()
		close(block)
		for range ch {
		}
		_ = sess.Close()
	}()

	loadCtx, cancelLoad := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.LoadModel(loadCtx, loadReq("b"))
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLoad()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled LoadModel while waiting = %v, want context.Canceled", err)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Active == nil || st.Active.ModelName != "a" || st.Active.Generation != active.Generation {
		t.Fatalf("active changed after canceled load: %+v", st)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestUnit_Slot_IdleReaperReleasesIdleModelThenReopens(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(5*time.Minute), WithClock(clk.now))

	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	// Not yet idle: the reaper leaves the model resident.
	clk.advance(4 * time.Minute)
	svc.reapIdle()
	if st, _ := svc.Status(ctx); st.Active == nil {
		t.Fatalf("model reaped before TTL: %+v", st)
	}

	// Past the TTL: reaped, device memory freed (backend session closed).
	clk.advance(2 * time.Minute)
	svc.reapIdle()
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotIdle || st.Active != nil {
		t.Fatalf("status after idle reap = %+v, want Idle/no-active", st)
	}
	if len(backend.sessions) != 1 || !backend.sessions[0].closed {
		t.Fatalf("idle reap did not close the backend session: %+v", backend.sessions)
	}

	// The next request reopens transparently.
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession after reap: %v", err)
	}
	defer sess.Close()
	if len(backend.opened) != 2 {
		t.Fatalf("expected a reopen after reap, backend.opened = %v", backend.opened)
	}
}

func TestUnit_Slot_IdleReaperReapsHeldButIdleSession(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(time.Minute), WithClock(clk.now))

	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	// A warm-cache-style held handle (refs>0) that is nonetheless idle must still
	// be reclaimed — modeld cannot trust the client to release.
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	clk.advance(2 * time.Minute)
	svc.reapIdle()

	if st, _ := svc.Status(ctx); st.Active != nil {
		t.Fatalf("held-but-idle session was not reaped: %+v", st)
	}
	// The held handle is now stale; the client maps this to drop+reopen.
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{}); !errors.Is(err, transport.ErrSlotGenerationStale) {
		t.Fatalf("stale handle after reap = %v, want ErrSlotGenerationStale", err)
	}
}

func TestUnit_Slot_IdleReaperNeverReapsBusySlot(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(time.Minute), WithClock(clk.now))
	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	block := make(chan transport.StreamChunk)
	backend.sessions[0].decodeCh = block
	decodeCtx, cancelDecode := context.WithCancel(ctx)
	ch, err := sess.Decode(decodeCtx, transport.DecodeConfig{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// Even far past the TTL, an in-flight decode (op-lock held, SlotBusy) is never
	// reaped: the reaper's non-blocking op-lock acquire fails.
	clk.advance(10 * time.Minute)
	svc.reapIdle()
	if st, _ := svc.Status(ctx); st.Active == nil {
		t.Fatalf("busy slot was reaped: %+v", st)
	}
	if backend.sessions[0].closed {
		t.Fatalf("reaper closed a busy slot's backend session")
	}

	cancelDecode()
	close(block)
	for range ch {
	}
	_ = sess.Close()
}

func TestUnit_Slot_IdleReaperDisabledWhenTTLZero(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(0), WithClock(clk.now))
	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	clk.advance(24 * time.Hour)
	svc.reapIdle()
	if st, _ := svc.Status(ctx); st.Active == nil {
		t.Fatalf("TTL=0 must disable reaping, but model was reaped: %+v", st)
	}
	// StartReaper is also a no-op when disabled.
	svc.StartReaper(ctx)
}

func TestUnit_Slot_DecodeCancellationReleasesBusyWhenOutputNotDrained(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))
	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()

	src := make(chan transport.StreamChunk, 32)
	for i := 0; i < cap(src); i++ {
		src <- transport.StreamChunk{Text: "x"}
	}
	backend.sessions[0].decodeCh = src
	decodeCtx, cancelDecode := context.WithCancel(ctx)
	out, err := sess.Decode(decodeCtx, transport.DecodeConfig{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	_ = out // Intentionally do not drain it; cancellation must still unblock the slot.
	cancelDecode()

	deadline := time.After(2 * time.Second)
	for {
		st, err := svc.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State == transport.SlotReady {
			close(src)
			return
		}
		select {
		case <-deadline:
			t.Fatalf("slot stayed busy after decode cancellation: %+v", st)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestUnit_Slot_DescribeCreditsReclaimableBytesWhenSwitchWouldEvict(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{requiredBytes: 3 << 30}
	svc := New(backend, WithBackend("llama"))

	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if _, err := svc.Describe(ctx, req("b")); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	got := backend.lastDescribe()
	if got.ModelName != "b" {
		t.Fatalf("last describe model = %q, want b", got.ModelName)
	}
	if got.ReclaimableBytes != 3<<30 {
		t.Fatalf("ReclaimableBytes = %d, want resident model a's footprint (%d)", got.ReclaimableBytes, int64(3<<30))
	}
}

func TestUnit_Slot_DescribeSameIdentityReturnsResidentOpenInfo(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{requiredBytes: 3 << 30}
	svc := New(backend, WithBackend("llama"))

	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	describesAfterLoad := len(backend.describes)
	// A live recompute after this point would be encumbered by the resident
	// session; the stored open-time answer is the session's truth. Mutating the
	// fake's answer proves the response comes from the store, not a new call.
	backend.requiredBytes = 1
	info, err := svc.Describe(ctx, req("a"))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.RequiredBytes != 3<<30 {
		t.Fatalf("RequiredBytes = %d, want the open-time answer (%d), not a recompute", info.RequiredBytes, int64(3<<30))
	}
	if len(backend.describes) != describesAfterLoad {
		t.Fatalf("backend.Describe called %d extra time(s) for a same-identity request", len(backend.describes)-describesAfterLoad)
	}
}

func TestUnit_Slot_DescribeNoCreditWhenNothingResident(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{requiredBytes: 3 << 30}
	svc := New(backend, WithBackend("llama"))

	inbound := req("a")
	// An inbound value must be ignored: ReclaimableBytes is owner-set by contract.
	inbound.ReclaimableBytes = 42 << 30
	if _, err := svc.Describe(ctx, inbound); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	got := backend.lastDescribe()
	if got.ReclaimableBytes != 0 {
		t.Fatalf("ReclaimableBytes = %d, want 0 with an empty slot", got.ReclaimableBytes)
	}
}

func TestUnit_Slot_ImplicitSessionStaysResidentWithReaper(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(5*time.Minute))

	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotReady || st.Active == nil || st.Active.ModelName != "a" {
		t.Fatalf("status after release = %+v, want model kept resident for the reaper", st)
	}
	if backend.sessions[0].closed {
		t.Fatal("implicit session was closed on release despite an active idle reaper")
	}

	// The next one-shot turn reuses the resident session without a reload.
	next, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession reuse: %v", err)
	}
	defer next.Close()
	if len(backend.opened) != 1 {
		t.Fatalf("backend opens = %d, want 1 (warm reuse, no reload)", len(backend.opened))
	}
}

func TestUnit_Slot_EmbedEvictsIdleImplicitResidentModel(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(5*time.Minute))

	// One-shot chat turn: the reaper keeps the implicit session resident after release.
	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The next embed must not wait out the idle TTL: the idle implicit model is
	// evicted and the embed proceeds.
	if _, err := svc.Embed(ctx, transport.EmbedRequest{ModelName: "embed", Type: "llama", Path: "/models/embed", Text: "hello"}); err != nil {
		t.Fatalf("Embed after released chat turn = %v, want success", err)
	}
	if len(backend.embeds) != 1 {
		t.Fatalf("backend embeds = %d, want 1", len(backend.embeds))
	}
	if !backend.sessions[0].closed {
		t.Fatal("idle implicit session was not evicted for the embed")
	}
	st, _ := svc.Status(ctx)
	if st.State != transport.SlotEmpty || st.Active != nil {
		t.Fatalf("status after embed = %+v, want empty", st)
	}
}

func TestUnit_Slot_EmbedBusyWhileSessionHeld(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(5*time.Minute))

	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()
	if _, err := svc.Embed(ctx, transport.EmbedRequest{ModelName: "embed", Type: "llama", Path: "/models/embed", Text: "hello"}); !errors.Is(err, transport.ErrModelBusy) {
		t.Fatalf("Embed while session held = %v, want ErrModelBusy", err)
	}
	if backend.sessions[0].closed {
		t.Fatal("held session must not be evicted by an embed")
	}
}

func TestUnit_Slot_EmbedBusyWithExplicitModel(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithIdleTTL(5*time.Minute))

	if _, err := svc.LoadModel(ctx, loadReq("a")); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if _, err := svc.Embed(ctx, transport.EmbedRequest{ModelName: "embed", Type: "llama", Path: "/models/embed", Text: "hello"}); !errors.Is(err, transport.ErrModelBusy) {
		t.Fatalf("Embed with explicit model = %v, want ErrModelBusy", err)
	}
	st, _ := svc.Status(ctx)
	if st.Active == nil || st.Active.ModelName != "a" {
		t.Fatalf("explicit model evicted by embed: %+v", st)
	}
}

// --- Model path resolution (remote-node model identification) ---

func writeTestModelFile(t *testing.T, dir, name string) {
	t.Helper()
	modelDir := filepath.Join(dir, name)
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", modelDir, err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.gguf"), []byte("weights-"+name), 0o644); err != nil {
		t.Fatalf("write model file: %v", err)
	}
}

// emptyPathReq mirrors req(name) but leaves Path (and the caller-asserted
// Digest, which a real caller in this state has no way to know either) empty,
// as a remote runtime must: it cannot name a path on the node's filesystem.
func emptyPathReq(name string) transport.OpenSessionRequest {
	r := req(name)
	r.Path = ""
	r.Digest = ""
	return r
}

func TestUnit_Slot_OpenSessionResolvesPathFromModelsDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTestModelFile(t, dir, "a")

	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	sess, err := svc.OpenSession(ctx, emptyPathReq("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()

	if len(backend.opened) != 1 {
		t.Fatalf("backend.opened = %v, want 1 entry", backend.opened)
	}
	st, _ := svc.Status(ctx)
	wantPath := filepath.Join(dir, "a", "model.gguf")
	if st.Active == nil || st.Active.Path != wantPath {
		t.Fatalf("status.Active = %+v, want Path=%q", st.Active, wantPath)
	}
}

func TestUnit_Slot_OpenSessionUnresolvedModelReturnsErrModelNotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	_, err := svc.OpenSession(ctx, emptyPathReq("missing"))
	if !errors.Is(err, modelstore.ErrModelNotFound) {
		t.Fatalf("OpenSession = %v, want ErrModelNotFound", err)
	}
	if len(backend.opened) != 0 {
		t.Fatalf("backend must not be reached for an unresolved model, opened=%v", backend.opened)
	}
}

func TestUnit_Slot_OpenSessionExplicitPathBypassesResolution(t *testing.T) {
	// Regression: a request that already carries a Path (the legacy
	// runtime-resolved-path flow) must not be rewritten, even when
	// WithModelsDir points somewhere that doesn't contain the model.
	ctx := context.Background()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(t.TempDir()))

	sess, err := svc.OpenSession(ctx, req("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()

	st, _ := svc.Status(ctx)
	if st.Active == nil || st.Active.Path != "/models/a" {
		t.Fatalf("status.Active = %+v, want explicit Path unchanged", st.Active)
	}
}

func TestUnit_Slot_DescribeResolvesPathAndMatchesResidentSameIdentity(t *testing.T) {
	// Regression: Describe must resolve Path before the same-identity
	// comparison against the resident session, or a resident model opened via
	// resolution would never match a later Describe call with an empty Path,
	// permanently disabling the openInfo shortcut for that model.
	ctx := context.Background()
	dir := t.TempDir()
	writeTestModelFile(t, dir, "a")

	backend := &fakeBackend{requiredBytes: 42}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	sess, err := svc.OpenSession(ctx, emptyPathReq("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()

	info, err := svc.Describe(ctx, emptyPathReq("a"))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.EffectiveContext == 0 {
		t.Fatalf("Describe returned zero EffectiveContext, resident openInfo shortcut did not match")
	}
	// The backend must have been asked to describe with the resolved path, not
	// invoked a second time for this same-identity Describe (that's the whole
	// point of the shortcut) — but the earlier describeForOpen call did reach
	// it with the resolved path.
	last := backend.lastDescribe()
	if last.Path != filepath.Join(dir, "a", "model.gguf") {
		t.Fatalf("backend describe request Path = %q, want resolved path", last.Path)
	}
}

func TestUnit_Slot_LoadModelResolvesPathFromModelsDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTestModelFile(t, dir, "a")

	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	lr := loadReq("a")
	lr.Path = ""
	lr.Digest = ""
	active, err := svc.LoadModel(ctx, lr)
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	wantPath := filepath.Join(dir, "a", "model.gguf")
	if active.Path != wantPath {
		t.Fatalf("LoadModel active.Path = %q, want %q", active.Path, wantPath)
	}
}

func TestUnit_Slot_LoadModelUnresolvedModelReturnsErrModelNotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	lr := loadReq("missing")
	lr.Path = ""
	if _, err := svc.LoadModel(ctx, lr); !errors.Is(err, modelstore.ErrModelNotFound) {
		t.Fatalf("LoadModel = %v, want ErrModelNotFound", err)
	}
}

func TestUnit_Slot_EmbedResolvesPathFromModelsDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTestModelFile(t, dir, "embed-model")

	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"))
	svc.modelsDir = dir

	if _, err := svc.Embed(ctx, transport.EmbedRequest{ModelName: "embed-model", Type: "llama", Text: "hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(backend.embeds) != 1 {
		t.Fatalf("backend.embeds = %d, want 1", len(backend.embeds))
	}
	wantPath := filepath.Join(dir, "embed-model", "model.gguf")
	if backend.embeds[0].Path != wantPath {
		t.Fatalf("embed request Path = %q, want %q", backend.embeds[0].Path, wantPath)
	}
}

func TestUnit_Slot_EmbedUnresolvedModelReturnsErrModelNotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := &fakeBackend{}
	svc := New(backend, WithBackend("llama"), WithModelsDir(dir))

	_, err := svc.Embed(ctx, transport.EmbedRequest{ModelName: "missing", Type: "llama", Text: "hello"})
	if !errors.Is(err, modelstore.ErrModelNotFound) {
		t.Fatalf("Embed = %v, want ErrModelNotFound", err)
	}
	if len(backend.embeds) != 0 {
		t.Fatalf("backend must not be reached for an unresolved model, embeds=%v", backend.embeds)
	}
}

func TestUnit_Slot_ModelsDirDefaultsToDataRootSubdir(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	writeTestModelFile(t, filepath.Join(dataRoot, "models"), "a")

	backend := &fakeBackend{}
	// No WithModelsDir: must fall back to <dataRoot>/models.
	svc := New(backend, WithBackend("llama"), WithDataRoot(dataRoot))

	sess, err := svc.OpenSession(ctx, emptyPathReq("a"))
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer sess.Close()
}
