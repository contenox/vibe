// Package owner manages lease-based ownership of the local runtime's resident
// state. On Join, one instance becomes the owner — it holds the lease and renews
// it in the background — while others become followers that know who the owner
// is and, when advertised, where to reach it.
//
// The owner self-fences: if it cannot renew before the lease expires, Lost()
// fires and the caller MUST stop touching owned state, because another instance
// may have taken over.
package owner

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/contenox/contenox/liblease"
	"github.com/contenox/contenox/libtracker"
)

// Role is whether this instance owns the lease.
type Role int

const (
	// RoleFollower means another instance currently owns the lease.
	RoleFollower Role = iota
	// RoleOwner means this instance holds and renews the lease.
	RoleOwner
)

func (r Role) String() string {
	if r == RoleOwner {
		return "owner"
	}
	return "follower"
}

// Config controls a Join.
type Config struct {
	// LeasePath is the lease file; one owner per path (per user + data root).
	LeasePath string
	// TTL is the lease duration.
	TTL time.Duration
	// RenewInterval is how often the owner renews; defaults to TTL/3.
	RenewInterval time.Duration
	// Endpoint, if set, is advertised to followers via the lease record so they
	// can reach the owner transport. Empty means no endpoint is advertised.
	Endpoint string
	// Backend, if set, is the inference backend this owner serves ("llama" /
	// "openvino" / "none"), advertised via the lease so the runtime can tell which
	// mode the daemon is in without a network round-trip.
	Backend string
	// Meta carries additional holder metadata for specialized leases. Endpoint
	// and Backend, when set above, override same-named entries here.
	Meta map[string]string
	// Tracker reports renew-loop telemetry. Nil degrades to a Noop tracker.
	Tracker libtracker.ActivityTracker
}

// EndpointMetaKey is the lease metadata key used to advertise the owner's
// transport endpoint.
const EndpointMetaKey = "endpoint"

// BackendMetaKey is the lease metadata key used to advertise the inference
// backend the owner serves (mirrored by runtime/internal/modeldprobe).
const BackendMetaKey = "backend"

var (
	acquireLease = liblease.Acquire
	inspectLease = liblease.Inspect
)

// leaseMeta builds the lease metadata advertised to followers. Empty values are
// omitted so a non-serving owner publishes no endpoint/backend.
func leaseMeta(cfg Config) map[string]string {
	meta := map[string]string{}
	for k, v := range cfg.Meta {
		if k != "" && v != "" {
			meta[k] = v
		}
	}
	if cfg.Endpoint != "" {
		meta[EndpointMetaKey] = cfg.Endpoint
	}
	if cfg.Backend != "" {
		meta[BackendMetaKey] = cfg.Backend
	}
	return meta
}

// Owner is the result of a Join: either this instance (RoleOwner) with a running
// renew loop, or a handle describing the current owner (RoleFollower).
type Owner struct {
	role    Role
	id      string
	holder  liblease.Record
	tracker libtracker.ActivityTracker

	lease  *liblease.Lease
	cancel context.CancelFunc

	lost     chan struct{}
	lostErr  error
	lostOnce sync.Once
	relOnce  sync.Once
	relErr   error
}

// Join attempts to acquire ownership of the lease at cfg.LeasePath. If the lease
// is free it returns an Owner in RoleOwner and starts renewing in the
// background; if a live owner already holds it, it returns RoleFollower with the
// current holder. ctx bounds the renew loop's lifetime; cancelling it stops
// renewal. Call Release to relinquish the lease before the TTL expires.
func Join(ctx context.Context, cfg Config) (*Owner, error) {
	if cfg.TTL <= 0 {
		return nil, errors.New("owner: ttl must be positive")
	}
	tracker := cfg.Tracker
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	interval := cfg.RenewInterval
	if interval <= 0 {
		interval = cfg.TTL / 3
	}
	if interval <= 0 {
		interval = time.Second
	}

	var opts []liblease.Option
	if meta := leaseMeta(cfg); len(meta) > 0 {
		opts = append(opts, liblease.WithMeta(meta))
	}

	var l *liblease.Lease
	for {
		var err error
		l, err = acquireLease(cfg.LeasePath, cfg.TTL, opts...)
		if err == nil {
			break
		}
		if !errors.Is(err, liblease.ErrHeld) {
			return nil, err
		}
		rec, ierr := inspectLease(cfg.LeasePath)
		if errors.Is(ierr, os.ErrNotExist) {
			// The holder released (or its file vanished) between the failed Acquire
			// and the Inspect. Retry after a short pause so a pathological race
			// (repeated held-then-gone) cannot spin the CPU.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(minRenewRetryInterval):
			}
			continue
		}
		if ierr != nil {
			return nil, ierr
		}
		return &Owner{
			role:    RoleFollower,
			id:      rec.InstanceID,
			holder:  rec,
			tracker: tracker,
			lost:    make(chan struct{}), // never fires for a follower
		}, nil
	}

	rctx, cancel := context.WithCancel(ctx)
	o := &Owner{
		role:    RoleOwner,
		id:      l.InstanceID(),
		holder:  l.Record(),
		tracker: tracker,
		lease:   l,
		cancel:  cancel,
		lost:    make(chan struct{}),
	}
	go o.renewLoop(rctx, cfg.TTL, interval)
	return o, nil
}

// minRenewRetryInterval floors the retry cadence so a near-expiry burst of
// failing renews cannot spin the CPU.
const minRenewRetryInterval = 250 * time.Millisecond

// renewLoop renews the lease on the normal interval and, crucially, does NOT
// forfeit ownership on a single transient renew failure (an fs/IO blip). It
// keeps retrying — more aggressively as expiry nears — and only declares the
// lease lost when either the holder is definitively gone (liblease.ErrLost: a
// takeover or a locally-expired lease) or it can no longer PROVE it still holds
// the lease (within renewGiveUpMargin of its own validity end). Giving up before
// expiry preserves the self-fence: a challenger can only Acquire after the lease
// lapses, so the owner never keeps touching state past the point another
// instance could take over.
func (o *Owner) renewLoop(ctx context.Context, ttl, interval time.Duration) {
	margin := renewGiveUpMargin(ttl)
	// validUntil is the wall-clock time our last successful renew (or the initial
	// acquisition) guarantees we still hold the lease.
	validUntil := o.holder.ExpiresAt()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return // graceful stop (parent ctx or Release); not a loss
		case <-timer.C:
		}

		attemptStart := time.Now()
		err := o.lease.Renew()
		if err == nil {
			validUntil = attemptStart.Add(ttl)
			timer.Reset(interval)
			continue
		}
		// A definitive loss — taken over, or our lease already expired. Retrying
		// cannot help and is unsafe; self-fence now.
		if errors.Is(err, liblease.ErrLost) {
			o.markLost(err)
			return
		}
		// Transient failure: keep the lease until we can no longer prove we hold it.
		if renewGaveUp(time.Now(), validUntil, margin) {
			o.markLost(err)
			return
		}
		retry := renewRetryDelay(interval, time.Until(validUntil))
		reportErr, _, end := o.tracker.Start(ctx, "modeld_owner", "lease_renew_retry",
			"instance", o.id, "retry_in", retry, "valid_until", validUntil)
		reportErr(err)
		end()
		timer.Reset(retry)
	}
}

// renewGiveUpMargin is how long before its own validity end the owner stops
// retrying a failing renew and forfeits. It must be positive so the owner always
// self-fences before expiry (with headroom for clock skew between the owner and
// a challenger), never after.
func renewGiveUpMargin(ttl time.Duration) time.Duration {
	return max(ttl/4, minRenewRetryInterval)
}

// renewGaveUp reports whether the owner can no longer prove it holds the lease:
// the safety margin before validUntil has elapsed, so it must self-fence rather
// than keep retrying.
func renewGaveUp(now, validUntil time.Time, margin time.Duration) bool {
	return !now.Add(margin).Before(validUntil)
}

// renewRetryDelay picks the wait before retrying a failed-but-not-lost renew. It
// never waits longer than the normal interval and grows more aggressive as the
// remaining validity shrinks (remaining/2), floored so it cannot spin.
func renewRetryDelay(interval, remaining time.Duration) time.Duration {
	return max(min(remaining/2, interval), minRenewRetryInterval)
}

func (o *Owner) markLost(err error) {
	o.lostOnce.Do(func() {
		o.lostErr = err
		close(o.lost)
	})
}

// Role reports whether this instance is the owner or a follower.
func (o *Owner) Role() Role { return o.role }

// IsOwner reports whether this instance holds the lease.
func (o *Owner) IsOwner() bool { return o.role == RoleOwner }

// InstanceID is this owner's id, or the current owner's id for a follower.
func (o *Owner) InstanceID() string { return o.id }

// Holder returns the current owner's lease record.
func (o *Owner) Holder() liblease.Record { return o.holder }

// Endpoint returns the owner's advertised endpoint, or "" if none.
func (o *Owner) Endpoint() string { return o.holder.Meta[EndpointMetaKey] }

// Backend returns the inference backend the owner serves, or "" if none.
func (o *Owner) Backend() string { return o.holder.Meta[BackendMetaKey] }

// Lost is closed when the owner can no longer renew the lease (taken over or an
// I/O failure). The caller must stop touching owned state when it fires. It
// never fires for a follower.
func (o *Owner) Lost() <-chan struct{} { return o.lost }

// LostErr returns why ownership was lost, after Lost fires.
func (o *Owner) LostErr() error { return o.lostErr }

// Release stops renewing and relinquishes the lease so a successor can take over
// immediately rather than waiting out the TTL. Safe to call multiple times; a
// no-op for a follower.
func (o *Owner) Release() error {
	o.relOnce.Do(func() {
		if o.cancel != nil {
			o.cancel()
		}
		if o.lease != nil {
			o.relErr = o.lease.Release()
		}
	})
	return o.relErr
}
