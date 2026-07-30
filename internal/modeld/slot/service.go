// Package slot enforces modeld's single active local model invariant.
package slot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/modeld/modelstore"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
	"github.com/contenox/contenox/libtracker"
)

// Service wraps a backend transport.Service and exposes it as one active model
// slot. The wrapped backend still owns native inference; this layer owns
// residency cardinality, generation fencing, and switch/unload safety.
type Service struct {
	backend transport.Service

	owner       string
	backendName string

	op chan struct{}
	mu sync.Mutex

	active     *activeSlot
	generation uint64
	state      transport.SlotState
	busyOp     string
	lastErr    string

	// idleTTL is how long a resident model may sit idle (no EnsurePrefix /
	// PrefillSuffix / Decode / Embed) before the reaper releases it to free device
	// memory. Zero disables idle reaping. now is injectable for tests.
	idleTTL      time.Duration
	lastActivity time.Time
	now          func() time.Time
	dataRoot     string
	// modelsDir overrides where resolveModelPath looks for models when a
	// request omits Path. Empty means <dataRoot>/<modelstore.DefaultSubdir>.
	modelsDir string
	// admin implements transport.NodeAdmin over the same models directory
	// resolveModelPath resolves against. Constructed once in New so its
	// per-model in-flight push guard is shared across every admin call.
	admin *modelstore.Admin

	tracker libtracker.ActivityTracker
}

type activeSlot struct {
	req        transport.OpenSessionRequest
	sess       transport.Session
	generation uint64
	refs       int
	explicit   bool
	// openInfo is the backend's capacity resolution captured immediately before
	// this session opened (post-eviction, unencumbered snapshot) — the same
	// inputs the session's own open resolved from. Describe returns it for
	// same-identity requests as the resident session's truth, and its
	// RequiredBytes is the reclaim credit for different-identity requests.
	// Immutable after construction; zero when the pre-open Describe failed.
	openInfo transport.ModelInfo
}

// Option configures a slot Service.
type Option func(*Service)

// WithOwner records the owner instance expected on direct calls. The gRPC layer
// also fences requests; this keeps direct tests and future in-process callers
// honest.
func WithOwner(owner string) Option {
	return func(s *Service) { s.owner = owner }
}

// WithBackend records the backend mode for Status.
func WithBackend(backend string) Option {
	return func(s *Service) { s.backendName = backend }
}

func WithDataRoot(root string) Option {
	return func(s *Service) { s.dataRoot = root }
}

// WithModelsDir overrides the models directory a request with an empty Path
// resolves against. Unset falls back to <dataRoot>/<modelstore.DefaultSubdir>.
func WithModelsDir(dir string) Option {
	return func(s *Service) { s.modelsDir = dir }
}

// WithIdleTTL sets how long a resident model may sit idle before the background
// reaper releases it (freeing device memory so the GPU can return to idle).
// Zero (the default) disables idle reaping. Reaping requires StartReaper.
func WithIdleTTL(ttl time.Duration) Option {
	return func(s *Service) { s.idleTTL = ttl }
}

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithTracker sets the ActivityTracker session-operation telemetry is
// reported through. Unset defaults to libtracker.NoopTracker.
func WithTracker(t libtracker.ActivityTracker) Option {
	return func(s *Service) {
		if t != nil {
			s.tracker = t
		}
	}
}

// New returns a transport service that allows only one active resident model.
func New(backend transport.Service, opts ...Option) *Service {
	s := &Service{backend: backend, op: make(chan struct{}, 1), state: transport.SlotEmpty, now: time.Now, tracker: libtracker.NoopTracker{}}
	for _, opt := range opts {
		opt(s)
	}
	s.lastActivity = s.now()
	s.admin = modelstore.NewAdmin(s.resolveModelsDir())
	return s
}

var _ transport.Service = (*Service)(nil)
var _ transport.ModelController = (*Service)(nil)
var _ transport.NodeAdmin = (*Service)(nil)

func (s *Service) Status(ctx context.Context) (transport.DaemonStatus, error) {
	if err := ctxErr(ctx); err != nil {
		return transport.DaemonStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked(), nil
}

func (s *Service) LoadModel(ctx context.Context, req transport.LoadModelRequest) (transport.ActiveModel, error) {
	if err := ctxErr(ctx); err != nil {
		return transport.ActiveModel{}, err
	}
	if err := s.checkFence(req.Fence.OwnerInstanceID); err != nil {
		return transport.ActiveModel{}, err
	}
	openReq := transport.OpenSessionRequest{
		Fence:     req.Fence,
		ModelName: req.ModelName,
		Type:      req.Type,
		Digest:    req.Digest,
		Path:      req.Path,
		Config:    req.Config,
		Adapters:  req.Adapters,
	}
	openReq, err := s.resolveModelPath(openReq)
	if err != nil {
		return transport.ActiveModel{}, err
	}

	unlock, err := s.lockOperation(ctx)
	if err != nil {
		return transport.ActiveModel{}, err
	}
	defer unlock()
	if err := ctxErr(ctx); err != nil {
		return transport.ActiveModel{}, err
	}

	s.mu.Lock()
	if req.ExpectedGeneration != 0 && req.ExpectedGeneration != s.generation {
		s.mu.Unlock()
		return transport.ActiveModel{}, transport.ErrSlotGenerationStale
	}
	if s.active != nil && sameIdentity(s.active.req, openReq) {
		s.active.explicit = true
		active := activeModel(s.active.req, s.active.generation)
		s.state = transport.SlotReady
		s.busyOp = ""
		s.lastErr = ""
		s.touchLocked()
		s.mu.Unlock()
		return active, nil
	}
	if s.active != nil && s.active.refs > 0 {
		s.mu.Unlock()
		return transport.ActiveModel{}, transport.ErrModelBusy
	}
	old := s.active
	if old != nil {
		s.active = nil
		s.generation++
		s.state = transport.SlotSwitching
		s.busyOp = "switch"
	} else {
		s.state = transport.SlotLoading
		s.busyOp = "load"
	}
	s.lastErr = ""
	s.mu.Unlock()

	if old != nil {
		if err := old.sess.Close(); err != nil {
			return transport.ActiveModel{}, s.finishCloseError(err)
		}
	}

	openInfo := s.describeForOpen(ctx, openReq)

	sess, err := s.backend.OpenSession(ctx, openReq)
	if err != nil {
		if sess != nil {
			_ = sess.Close()
		}
		return transport.ActiveModel{}, s.finishLoadError(loadError(err))
	}

	s.mu.Lock()
	s.generation++
	gen := s.generation
	s.active = &activeSlot{req: openReq, sess: sess, generation: gen, explicit: true, openInfo: openInfo}
	s.state = transport.SlotReady
	s.busyOp = ""
	s.lastErr = ""
	s.touchLocked()
	active := activeModel(openReq, gen)
	s.mu.Unlock()
	return active, nil
}

func (s *Service) UnloadModel(ctx context.Context, req transport.UnloadModelRequest) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := s.checkFence(req.Fence.OwnerInstanceID); err != nil {
		return err
	}

	unlock, err := s.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	if req.ExpectedGeneration != 0 && req.ExpectedGeneration != s.generation {
		s.mu.Unlock()
		return transport.ErrSlotGenerationStale
	}
	if s.active == nil {
		s.state = transport.SlotEmpty
		s.busyOp = ""
		s.lastErr = ""
		s.mu.Unlock()
		return nil
	}
	if s.active.refs > 0 {
		s.mu.Unlock()
		return transport.ErrModelBusy
	}
	old := s.active
	s.active = nil
	s.generation++
	s.state = transport.SlotUnloading
	s.busyOp = "unload"
	s.lastErr = ""
	s.mu.Unlock()

	err = old.sess.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state = transport.SlotFailed
		s.busyOp = ""
		s.lastErr = err.Error()
		return err
	}
	s.state = transport.SlotEmpty
	s.busyOp = ""
	s.lastErr = ""
	return nil
}

func (s *Service) OpenSession(ctx context.Context, req transport.OpenSessionRequest) (transport.Session, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := s.checkFence(req.Fence.OwnerInstanceID); err != nil {
		return nil, err
	}
	req, err := s.resolveModelPath(req)
	if err != nil {
		return nil, err
	}

	unlock, err := s.lockOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.active != nil {
		switch {
		case sameIdentity(s.active.req, req):
			if s.active.refs > 0 {
				s.mu.Unlock()
				return nil, transport.ErrModelBusy
			}
			s.active.refs = 1
			s.state = transport.SlotReady
			s.busyOp = ""
			s.lastErr = ""
			s.touchLocked()
			gen := s.active.generation
			s.mu.Unlock()
			return &slotSession{svc: s, generation: gen}, nil
		case s.active.refs > 0:
			s.mu.Unlock()
			return nil, transport.ErrModelBusy
		default:
			s.mu.Unlock()
			return nil, transport.ErrModelSwitchRequired
		}
	}
	s.state = transport.SlotLoading
	s.busyOp = "load"
	s.lastErr = ""
	s.mu.Unlock()

	openInfo := s.describeForOpen(ctx, req)

	sess, err := s.backend.OpenSession(ctx, req)
	if err != nil {
		if sess != nil {
			_ = sess.Close()
		}
		return nil, s.finishLoadError(loadError(err))
	}

	s.mu.Lock()
	s.generation++
	gen := s.generation
	s.active = &activeSlot{req: req, sess: sess, generation: gen, refs: 1, openInfo: openInfo}
	s.state = transport.SlotReady
	s.busyOp = ""
	s.lastErr = ""
	s.touchLocked()
	s.mu.Unlock()
	return &slotSession{svc: s, generation: gen}, nil
}

// describeForOpen captures the backend's capacity resolution for the session
// about to open. It runs after any old session closed and before the new one
// allocates, so the snapshot is unencumbered — the same picture the open itself
// resolves from. Best-effort: a failure just yields a zero ModelInfo, which
// disables the same-identity shortcut and the reclaim credit.
func (s *Service) describeForOpen(ctx context.Context, req transport.OpenSessionRequest) transport.ModelInfo {
	req.ReclaimableBytes = 0
	info, err := s.backend.Describe(ctx, req)
	if err != nil {
		return transport.ModelInfo{}
	}
	return info
}

// Describe stays side-effect-free: it never evicts the resident session.
//   - Same identity resident: the truthful answer is what that session actually
//     resolved at open — a live recompute would be encumbered by the session's
//     own footprint and report a uselessly small hypothetical (the "panel shows
//     439 while the session serves 2891" bug).
//   - Different identity resident: pass the resident session's footprint as
//     ReclaimableBytes so the backend's free-memory snapshot reflects what an
//     actual OpenSession would see after the switch closed the old session.
func (s *Service) Describe(ctx context.Context, req transport.OpenSessionRequest) (transport.ModelInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return transport.ModelInfo{}, err
	}
	if err := s.checkFence(req.Fence.OwnerInstanceID); err != nil {
		return transport.ModelInfo{}, err
	}
	req, err := s.resolveModelPath(req)
	if err != nil {
		return transport.ModelInfo{}, err
	}
	// ReclaimableBytes is owner-set by contract; never trust an inbound value.
	req.ReclaimableBytes = 0
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil {
		if sameIdentity(active.req, req) {
			if active.openInfo.EffectiveContext > 0 {
				return active.openInfo, nil
			}
		} else {
			req.ReclaimableBytes = active.openInfo.RequiredBytes
		}
	}
	return s.backend.Describe(ctx, req)
}

func (s *Service) Embed(ctx context.Context, req transport.EmbedRequest) (transport.EmbedResult, error) {
	if err := ctxErr(ctx); err != nil {
		return transport.EmbedResult{}, err
	}
	if err := s.checkFence(req.Fence.OwnerInstanceID); err != nil {
		return transport.EmbedResult{}, err
	}
	req, err := s.resolveEmbedPath(req)
	if err != nil {
		return transport.EmbedResult{}, err
	}
	unlock, err := s.lockOperation(ctx)
	if err != nil {
		return transport.EmbedResult{}, err
	}
	defer unlock()
	if err := ctxErr(ctx); err != nil {
		return transport.EmbedResult{}, err
	}
	s.mu.Lock()
	if s.active != nil {
		// An idle implicit session is only resident because the idle reaper (not
		// release) owns reclamation now; before that change release would already
		// have closed it and this embed would have found the slot empty. Evict it
		// and proceed. In-use sessions and explicitly loaded models keep reporting
		// busy, exactly as before.
		if s.active.refs > 0 || s.active.explicit {
			s.mu.Unlock()
			return transport.EmbedResult{}, transport.ErrModelBusy
		}
		old := s.active
		s.active = nil
		s.generation++
		s.state = transport.SlotUnloading
		s.busyOp = "embed"
		s.lastErr = ""
		s.mu.Unlock()
		if err := old.sess.Close(); err != nil {
			return transport.EmbedResult{}, s.finishCloseError(err)
		}
		s.mu.Lock()
	}
	s.state = transport.SlotBusy
	s.busyOp = "embed"
	s.mu.Unlock()
	res, err := s.backend.Embed(ctx, req)
	s.markReadyOrError(err)
	return res, err
}

func (s *Service) lockOperation(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.op <- struct{}{}:
		return func() { <-s.op }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func loadError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", transport.ErrModelLoadFailed, err)
}

func (s *Service) finishLoadError(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busyOp = ""
	s.lastErr = err.Error()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.state = transport.SlotEmpty
		return err
	}
	s.state = transport.SlotFailed
	return err
}

func (s *Service) finishCloseError(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = transport.SlotFailed
	s.busyOp = ""
	s.lastErr = err.Error()
	return err
}

func (s *Service) statusLocked() transport.DaemonStatus {
	state := s.state
	if state == "" {
		state = transport.SlotEmpty
	}
	st := transport.DaemonStatus{
		OwnerInstanceID: s.owner,
		Backend:         s.backendName,
		State:           state,
		BusyOperation:   s.busyOp,
		LastError:       s.lastErr,
	}
	if s.active != nil {
		active := activeModel(s.active.req, s.active.generation)
		st.Active = &active
	}
	return st
}

func (s *Service) checkFence(owner string) error {
	if s.owner == "" {
		return nil
	}
	if owner != s.owner {
		return transport.ErrStaleFence
	}
	return nil
}

func (s *Service) release(gen uint64) error {
	unlock, err := s.lockOperation(context.Background())
	if err != nil {
		return err
	}
	defer unlock()

	s.mu.Lock()
	if s.active == nil || s.active.generation != gen {
		s.mu.Unlock()
		return nil
	}
	if s.active.refs > 0 {
		s.active.refs--
	}
	// Keep implicit sessions resident after the handle is released whenever the
	// idle reaper owns reclamation: closing here throws away warm KV (hot and
	// cold) that the very next one-shot CLI turn would reuse — the measured 10x
	// warm-turn win. The reaper frees the device after the idle TTL regardless
	// of how the session was opened. Without a reaper (idleTTL <= 0) release
	// stays the only reclamation point, so the old close-on-release behavior is
	// preserved.
	if s.active.refs > 0 || s.active.explicit || s.idleTTL > 0 {
		s.state = transport.SlotReady
		s.busyOp = ""
		s.touchLocked()
		s.mu.Unlock()
		return nil
	}
	old := s.active
	s.active = nil
	s.generation++
	s.state = transport.SlotUnloading
	s.busyOp = "unload"
	s.lastErr = ""
	s.mu.Unlock()

	err = old.sess.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state = transport.SlotFailed
		s.busyOp = ""
		s.lastErr = err.Error()
		return err
	}
	s.state = transport.SlotEmpty
	s.busyOp = ""
	s.lastErr = ""
	return nil
}

// StartReaper runs the idle-reaper loop until ctx is canceled. While modeld holds
// the lease it periodically releases a resident model that has sat idle past the
// configured TTL, so device memory is freed and the GPU can return to idle (the
// 24/7-on-a-laptop case). It is a no-op when the TTL is zero. The tick is
// CPU-only and never touches the GPU.
func (s *Service) StartReaper(ctx context.Context) {
	if s.idleTTL <= 0 {
		return
	}
	interval := max(s.idleTTL/3, time.Second)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reapIdle()
			}
		}
	}()
}

// reapIdle releases the resident model when it has been idle past the TTL. It
// reaps regardless of open session handles — a warm-cache-pinned but idle session
// is exactly what must be reclaimed: the generation bump invalidates stale handles
// and clients reopen transparently on their next call (ErrSlotGenerationStale).
// It never interrupts work: a non-blocking op-lock acquire plus the SlotReady
// guard guarantee no operation is in flight. Tests call it directly.
func (s *Service) reapIdle() {
	if s.idleTTL <= 0 {
		return
	}
	// Non-blocking op-lock: if any operation holds it, the slot is in use this
	// tick; skip and re-check next tick.
	select {
	case s.op <- struct{}{}:
	default:
		return
	}
	defer func() { <-s.op }()

	s.mu.Lock()
	if s.active == nil || s.state != transport.SlotReady {
		s.mu.Unlock()
		return
	}
	if s.now().Sub(s.lastActivity) < s.idleTTL {
		s.mu.Unlock()
		return
	}
	old := s.active
	s.active = nil
	s.generation++
	s.state = transport.SlotIdle
	s.busyOp = "idle_reap"
	s.lastErr = ""
	s.mu.Unlock()

	err := old.sess.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.busyOp = ""
	if err != nil {
		s.state = transport.SlotFailed
		s.lastErr = err.Error()
		return
	}
	s.state = transport.SlotIdle
}

func (s *Service) withSession(ctx context.Context, gen uint64, op string, fn func(transport.Session) error) error {
	unlock, err := s.lockOperation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}

	sess, err := s.beginOperation(gen, op)
	if err != nil {
		s.logOperationFailure(ctx, op, gen, err)
		return err
	}
	err = fn(sess)
	s.logOperationFailure(ctx, op, gen, err)
	s.markReadyOrError(err)
	return err
}

// touchLocked records session activity so the idle reaper defers reaping.
// Callers must hold s.mu.
func (s *Service) touchLocked() { s.lastActivity = s.now() }

func (s *Service) beginOperation(gen uint64, op string) (transport.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		if gen != 0 && gen != s.generation {
			return nil, transport.ErrSlotGenerationStale
		}
		return nil, transport.ErrModelNotActive
	}
	if s.active.generation != gen {
		return nil, transport.ErrSlotGenerationStale
	}
	s.state = transport.SlotBusy
	s.busyOp = op
	s.lastErr = ""
	s.touchLocked()
	return s.active.sess, nil
}

func (s *Service) markReadyOrError(err error) {
	var closeSess transport.Session
	s.mu.Lock()
	s.busyOp = ""
	s.touchLocked()
	if err != nil && !errors.Is(err, context.Canceled) {
		s.lastErr = err.Error()
		if errors.Is(err, transport.ErrSessionFatal) {
			if s.active != nil {
				closeSess = s.active.sess
				s.active = nil
				s.generation++
			}
			s.state = transport.SlotFailed
			s.mu.Unlock()
			if closeSess != nil {
				_ = closeSess.Close()
			}
			return
		}
		if errors.Is(err, transport.ErrSessionClosed) || errors.Is(err, transport.ErrSlotGenerationStale) {
			s.state = transport.SlotFailed
			s.mu.Unlock()
			return
		}
	}
	if s.active == nil {
		s.state = transport.SlotEmpty
		s.mu.Unlock()
		return
	}
	s.state = transport.SlotReady
	s.mu.Unlock()
}

func (s *Service) logOperationFailure(ctx context.Context, op string, gen uint64, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}

	s.mu.Lock()
	state := s.state
	busyOp := s.busyOp
	modelName := ""
	modelType := ""
	modelDigest := ""
	if s.active != nil {
		modelName = s.active.req.ModelName
		modelType = s.active.req.Type
		modelDigest = s.active.req.Digest
	}
	s.mu.Unlock()

	reportErr, _, end := s.tracker.Start(ctx, "modeld_slot", "session_operation_failed",
		"op", op,
		"generation", gen,
		"backend", s.backendName,
		"state", state,
		"busy_op", busyOp,
		"class", operationErrorClass(err),
		"model", modelName,
		"type", modelType,
		"digest", modelDigest,
	)
	reportErr(err)
	end()
}

func operationErrorClass(err error) string {
	switch {
	case errors.Is(err, transport.ErrSessionFatal):
		return "session_fatal"
	case errors.Is(err, contextasm.ErrManifestMismatch):
		return "manifest_mismatch"
	case errors.Is(err, transport.ErrContextOverflow):
		return "context_overflow"
	case errors.Is(err, transport.ErrSlotGenerationStale):
		return "slot_generation_stale"
	case errors.Is(err, transport.ErrSessionClosed):
		return "session_closed"
	case errors.Is(err, transport.ErrModelNotActive):
		return "model_not_active"
	default:
		return "session_error"
	}
}

// resolveModelsDir returns the models directory used for path resolution and
// admin operations: the explicit override, or <dataRoot>/<modelstore.DefaultSubdir>.
func (s *Service) resolveModelsDir() string {
	return modelstore.Dir(s.dataRoot, s.modelsDir)
}

// resolvePath fills in a model's on-disk path from the node's local models
// directory when the caller left path empty. A remote runtime has no way to
// name a path on the node's filesystem, so this is how a request identifies a
// model deterministically by name+backendType(+digest) instead. An explicit,
// non-empty path is returned unchanged — this keeps the legacy
// runtime-resolved-path flow working during the local/remote transition.
func (s *Service) resolvePath(path, name, backendType, digest string) (string, error) {
	if path != "" {
		return path, nil
	}
	return modelstore.Resolve(s.resolveModelsDir(), name, backendType, digest)
}

// ListModels implements transport.NodeAdmin by delegating to the node's model
// store admin.
func (s *Service) ListModels(ctx context.Context) ([]transport.NodeModel, error) {
	models, err := s.admin.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]transport.NodeModel, len(models))
	for i, m := range models {
		out[i] = transport.NodeModel{Name: m.Name, Type: m.Type, Digest: m.Digest, SizeBytes: m.SizeBytes, ContextLength: m.ContextLength}
	}
	return out, nil
}

// RemoveModel implements transport.NodeAdmin. It rejects the deletion if the
// target model is currently resident in the slot, avoiding underneath-the-GPU
// file removal.
func (s *Service) RemoveModel(ctx context.Context, name string) error {
	s.mu.Lock()
	if s.active != nil && s.active.req.ModelName == name {
		s.mu.Unlock()
		return fmt.Errorf("cannot remove model %q: %w", name, transport.ErrModelBusy)
	}
	s.mu.Unlock()
	return s.admin.RemoveModel(ctx, name)
}

// DiskStats implements transport.NodeAdmin.
func (s *Service) DiskStats(ctx context.Context) (transport.NodeDiskStats, error) {
	st, err := s.admin.DiskStats(ctx)
	if err != nil {
		return transport.NodeDiskStats{}, err
	}
	return transport.NodeDiskStats{FreeBytes: st.FreeBytes, UsedBytes: st.UsedBytes, TotalBytes: st.TotalBytes}, nil
}

// ReceiveModel implements transport.NodeAdmin.
func (s *Service) ReceiveModel(ctx context.Context, manifest transport.PushManifest, r io.Reader) (transport.PushResult, error) {
	res, err := s.admin.ReceiveModel(ctx, modelstore.PushManifest{
		Name:       manifest.Name,
		Type:       manifest.Type,
		Digest:     manifest.Digest,
		TotalBytes: manifest.TotalBytes,
		Format:     modelstore.PushFormat(manifest.Format),
	}, r)
	if err != nil {
		return transport.PushResult{}, err
	}
	return transport.PushResult{AlreadyPresent: res.AlreadyPresent, BytesWritten: res.BytesWritten}, nil
}

// resolveModelPath is resolvePath specialized for OpenSessionRequest/LoadModelRequest callers.
func (s *Service) resolveModelPath(req transport.OpenSessionRequest) (transport.OpenSessionRequest, error) {
	path, err := s.resolvePath(req.Path, req.ModelName, req.Type, req.Digest)
	if err != nil {
		return req, err
	}
	req.Path = path
	return req, nil
}

// resolveEmbedPath is resolvePath specialized for EmbedRequest callers.
func (s *Service) resolveEmbedPath(req transport.EmbedRequest) (transport.EmbedRequest, error) {
	path, err := s.resolvePath(req.Path, req.ModelName, req.Type, req.Digest)
	if err != nil {
		return req, err
	}
	req.Path = path
	return req, nil
}

func sameIdentity(a, b transport.OpenSessionRequest) bool {
	return a.ModelName == b.ModelName &&
		a.Type == b.Type &&
		a.Digest == b.Digest &&
		a.Path == b.Path &&
		reflect.DeepEqual(a.Config, b.Config) &&
		reflect.DeepEqual(a.Adapters, b.Adapters)
}

func activeModel(req transport.OpenSessionRequest, gen uint64) transport.ActiveModel {
	return transport.ActiveModel{
		ModelName:  req.ModelName,
		Type:       req.Type,
		Digest:     req.Digest,
		Path:       req.Path,
		Config:     req.Config,
		Adapters:   req.Adapters,
		Generation: gen,
	}
}

type slotSession struct {
	svc        *Service
	generation uint64

	mu     sync.Mutex
	closed bool
}

var _ transport.Session = (*slotSession)(nil)

func (s *slotSession) SlotGeneration() uint64 { return s.generation }

func (s *slotSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *slotSession) EnsurePrefix(ctx context.Context, prefix transport.PrefixInput) (transport.PrefixStatus, error) {
	if s.isClosed() {
		return transport.PrefixStatus{}, transport.ErrSessionClosed
	}
	var out transport.PrefixStatus
	err := s.svc.withSession(ctx, s.generation, "ensure_prefix", func(sess transport.Session) error {
		var err error
		out, err = sess.EnsurePrefix(ctx, prefix)
		return err
	})
	return out, err
}

func (s *slotSession) PrefillSuffix(ctx context.Context, suffix transport.SuffixInput) (transport.SuffixStatus, error) {
	if s.isClosed() {
		return transport.SuffixStatus{}, transport.ErrSessionClosed
	}
	var out transport.SuffixStatus
	err := s.svc.withSession(ctx, s.generation, "prefill_suffix", func(sess transport.Session) error {
		var err error
		out, err = sess.PrefillSuffix(ctx, suffix)
		return err
	})
	return out, err
}

func (s *slotSession) Decode(ctx context.Context, cfg transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	if s.isClosed() {
		return nil, transport.ErrSessionClosed
	}
	unlock, err := s.svc.lockOperation(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		unlock()
		return nil, err
	}
	sess, err := s.svc.beginOperation(s.generation, "decode")
	if err != nil {
		unlock()
		s.svc.logOperationFailure(ctx, "decode", s.generation, err)
		return nil, err
	}
	src, err := sess.Decode(ctx, cfg)
	if err != nil {
		s.svc.logOperationFailure(ctx, "decode", s.generation, err)
		s.svc.markReadyOrError(err)
		unlock()
		return nil, err
	}
	out := make(chan transport.StreamChunk, 16)
	go func() {
		defer unlock()
		defer close(out)
		var streamErr error
		for chunk := range src {
			if chunk.Error != nil {
				streamErr = chunk.Error
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				streamErr = ctx.Err()
				s.svc.logOperationFailure(ctx, "decode", s.generation, streamErr)
				s.svc.markReadyOrError(streamErr)
				return
			}
		}
		s.svc.logOperationFailure(ctx, "decode", s.generation, streamErr)
		s.svc.markReadyOrError(streamErr)
	}()
	return out, nil
}

func (s *slotSession) ExplainContext() transport.ContextReport {
	if s.isClosed() {
		return transport.ContextReport{Closed: true}
	}
	var out transport.ContextReport
	err := s.svc.withSession(context.Background(), s.generation, "explain_context", func(sess transport.Session) error {
		out = sess.ExplainContext()
		return nil
	})
	if err != nil {
		return transport.ContextReport{Closed: true}
	}
	return out
}

func (s *slotSession) blobDir() string {
	d := s.svc.dataRoot
	if d == "" {
		return ""
	}
	return filepath.Join(d, "modeld-blobs", s.svc.backendName)
}

func (s *slotSession) Snapshot(ctx context.Context) (transport.SessionSnapshot, error) {
	if s.isClosed() {
		return transport.SessionSnapshot{}, transport.ErrSessionClosed
	}
	var out transport.SessionSnapshot
	err := s.svc.withSession(ctx, s.generation, "snapshot", func(sess transport.Session) error {
		var err error
		out, err = sess.Snapshot(ctx)
		return err
	})
	if err == nil && len(out.State) > 0 && s.svc.dataRoot != "" {
		hash := sha256.Sum256(out.State)
		stateID := hex.EncodeToString(hash[:])
		dir := s.blobDir()
		if err := os.MkdirAll(dir, 0700); err == nil {
			path := filepath.Join(dir, stateID+".bin")
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, out.State, 0600); err == nil {
				os.Rename(tmp, path)
				out.StateID = stateID
				out.State = nil
			} else {
				reportErr, _, end := s.svc.tracker.Start(ctx, "modeld_slot", "snapshot_blob_write_failed", "path", tmp)
				reportErr(err)
				end()
			}
		}
	}
	return out, err
}

func (s *slotSession) Restore(ctx context.Context, snap transport.SessionSnapshot) error {
	if s.isClosed() {
		return transport.ErrSessionClosed
	}
	if snap.StateID != "" && len(snap.State) == 0 && s.svc.dataRoot != "" {
		path := filepath.Join(s.blobDir(), snap.StateID+".bin")
		if data, err := os.ReadFile(path); err == nil {
			snap.State = data
		} else {
			reportErr, _, end := s.svc.tracker.Start(ctx, "modeld_slot", "snapshot_blob_read_failed", "path", path)
			reportErr(err)
			end()
			return fmt.Errorf("snapshot blob not found: %w", err)
		}
	}
	return s.svc.withSession(ctx, s.generation, "restore", func(sess transport.Session) error {
		return sess.Restore(ctx, snap)
	})
}

func (s *slotSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.svc.release(s.generation)
}
