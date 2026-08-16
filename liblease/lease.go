// Package liblease implements a cooperative, time-bounded file lease: a
// single-holder lock backed by an ordinary JSON file that records who holds the
// lease and until when. A challenger may take over only once the lease has
// expired. A lease gives liveness, not safety: fence critical operations on the
// holder's InstanceID.
package liblease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrHeld is returned by Acquire when a live lease is held by a different instance.
var ErrHeld = errors.New("liblease: lease is held by another instance")

// ErrLost is returned by Renew when the lease has been taken over by another instance.
var ErrLost = errors.New("liblease: lease was taken over by another instance")

// ErrAcquireTimeout is returned when the wait for the internal acquisition lock
// exceeds its bound before this instance could evaluate the lease. It says
// nothing about who holds the lease.
var ErrAcquireTimeout = errors.New("liblease: timed out waiting for the acquisition lock")

// Record is the on-disk lease state. PID and Host are informational, InstanceID
// is the fencing token, and Meta carries optional holder-supplied discovery data.
type Record struct {
	InstanceID string            `json:"instance_id"`
	PID        int               `json:"pid"`
	Host       string            `json:"host"`
	AcquiredAt time.Time         `json:"acquired_at"`
	RenewedAt  time.Time         `json:"renewed_at"`
	TTL        time.Duration     `json:"ttl"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// Option customizes a lease at Acquire time.
type Option func(*Record)

// WithMeta attaches holder-supplied discovery data to the lease record. It is
// preserved across Renew.
func WithMeta(meta map[string]string) Option {
	return func(r *Record) { r.Meta = meta }
}

// ExpiresAt reports when the lease lapses.
func (r Record) ExpiresAt() time.Time { return r.RenewedAt.Add(r.TTL) }

func (r Record) expired(now time.Time) bool { return now.After(r.ExpiresAt()) }

// Lease is a held lease handle. It is safe for concurrent use.
type Lease struct {
	mu   sync.Mutex
	path string
	rec  Record
}

// Acquire claims the lease at path for ttl using context.Background. Prefer
// AcquireContext where a context is available.
func Acquire(path string, ttl time.Duration, opts ...Option) (*Lease, error) {
	return AcquireContext(context.Background(), path, ttl, opts...)
}

// AcquireContext claims the lease at path for ttl. It succeeds when the lease is
// free, expired, or unreadable, and fails with ErrHeld when a different instance
// holds a still-valid lease; on success the caller must Renew before ttl elapses
// and Release on shutdown.
func AcquireContext(ctx context.Context, path string, ttl time.Duration, opts ...Option) (*Lease, error) {
	if ttl <= 0 {
		return nil, errors.New("liblease: ttl must be positive")
	}
	unlock, err := acquireLeaseLock(ctx, path, ttl)
	if err != nil {
		return nil, err
	}
	defer unlock()

	now := time.Now()
	host, _ := os.Hostname()
	rec := Record{
		InstanceID: uuid.NewString(),
		PID:        os.Getpid(),
		Host:       host,
		AcquiredAt: now,
		RenewedAt:  now,
		TTL:        ttl,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&rec)
		}
	}

	won, err := tryCreate(path, rec)
	if err != nil {
		return nil, err
	}
	if won {
		return &Lease{path: path, rec: rec}, nil
	}

	// A corrupt or unreadable lease file is treated as stale.
	if cur, rerr := readRecord(path); rerr == nil && !cur.expired(now) {
		return nil, fmt.Errorf("%w: instance %s (pid %d) until %s",
			ErrHeld, cur.InstanceID, cur.PID, cur.ExpiresAt().Format(time.RFC3339))
	}

	if err := writeRecord(path, rec); err != nil {
		return nil, err
	}
	after, err := readRecord(path)
	if err != nil {
		return nil, err
	}
	if after.InstanceID != rec.InstanceID {
		return nil, fmt.Errorf("%w: lost takeover race to instance %s", ErrHeld, after.InstanceID)
	}
	return &Lease{path: path, rec: rec}, nil
}

const leaseLockStaleAfter = 5 * time.Second

const leaseLockMaxWait = 30 * time.Second

// Deliberately tight and un-backed-off; see reshape-rationale-salvage.md.
const leaseLockPollInterval = time.Millisecond

func acquireLeaseLock(ctx context.Context, path string, ttl time.Duration) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAcquireTimeout, err)
	}
	ctx, cancel := context.WithTimeout(ctx, leaseLockMaxWait)
	defer cancel()

	lockPath := path + ".acquire.lock"
	staleAfter := leaseLockStaleAfter
	if ttl > 0 && ttl < staleAfter {
		staleAfter = ttl
	}
	if staleAfter < 100*time.Millisecond {
		staleAfter = 100 * time.Millisecond
	}

	timer := time.NewTimer(leaseLockPollInterval)
	defer timer.Stop()

	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > staleAfter {
			_ = os.Remove(lockPath)
			continue
		} else if statErr != nil && errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return nil, statErr
		}

		timer.Reset(leaseLockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w: %q: %w", ErrAcquireTimeout, lockPath, ctx.Err())
		case <-timer.C:
		}
	}
}

// tryCreate creates the lease file only if absent, via temp file + hardlink. It
// returns (false, nil) when the file already exists.
func tryCreate(path string, rec Record) (bool, error) {
	tmpName, err := writeTemp(path, rec)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpName)
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Renew extends the lease by another TTL from now, using context.Background.
// Prefer RenewContext where a context is available.
func (l *Lease) Renew() error { return l.RenewContext(context.Background()) }

// RenewContext extends the lease by another TTL from now. It returns ErrLost if
// the lease expired locally or was taken over by another instance; a context
// error surfaces as ErrAcquireTimeout, never as ErrLost.
func (l *Lease) RenewContext(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	unlock, err := acquireLeaseLock(ctx, l.path, l.rec.TTL)
	if err != nil {
		return fmt.Errorf("liblease: renew lock: %w", err)
	}
	defer unlock()

	cur, err := readRecord(l.path)
	if err != nil {
		return fmt.Errorf("liblease: renew: %w", err)
	}
	if cur.InstanceID != l.rec.InstanceID {
		return fmt.Errorf("%w: now held by %s", ErrLost, cur.InstanceID)
	}
	if l.rec.expired(time.Now()) {
		return fmt.Errorf("%w: lease expired before renewal", ErrLost)
	}
	l.rec.RenewedAt = time.Now()
	return writeRecord(l.path, l.rec)
}

// Release relinquishes the lease using context.Background. Prefer
// ReleaseContext where a context is available.
func (l *Lease) Release() error { return l.ReleaseContext(context.Background()) }

// ReleaseContext relinquishes the lease by removing the file, but only if it is
// still ours. It is safe to call more than once.
func (l *Lease) ReleaseContext(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	unlock, err := acquireLeaseLock(ctx, l.path, l.rec.TTL)
	if err != nil {
		return err
	}
	defer unlock()

	cur, err := readRecord(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if cur.InstanceID != l.rec.InstanceID {
		return nil // taken over already; not ours to remove
	}
	return os.Remove(l.path)
}

// Expired reports whether this holder's lease has lapsed based on its last
// successful Renew. A holder should use it to self-fence.
func (l *Lease) Expired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rec.expired(time.Now())
}

// InstanceID returns this holder's fencing token.
func (l *Lease) InstanceID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rec.InstanceID
}

// Record returns a snapshot of this holder's lease state.
func (l *Lease) Record() Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rec
}

// Inspect reads the current lease at path without acquiring it. It returns
// os.ErrNotExist when no lease file is present.
func Inspect(path string) (Record, error) {
	return InspectContext(context.Background(), path)
}

// InspectContext reads the current lease at path without acquiring it. It takes
// no acquisition lock, so it never waits on a peer.
func InspectContext(ctx context.Context, path string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	return readRecord(path)
}

func readRecord(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("liblease: corrupt lease %q: %w", path, err)
	}
	return rec, nil
}

// writeRecord writes rec atomically via temp file + rename.
func writeRecord(path string, rec Record) error {
	tmpName, err := writeTemp(path, rec)
	if err != nil {
		return err
	}
	defer os.Remove(tmpName)
	return os.Rename(tmpName, path)
}

// writeTemp marshals rec to a fresh temp file next to path and returns its name.
func writeTemp(path string, rec Record) (string, error) {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lease-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}
