// Package devicelease guards physical accelerator residency across modeld data
// roots. The normal modeld lease stays per-data-root for service discovery; this
// wrapper adds a second, fixed-location lease keyed by the backend's resolved
// accelerator device so two daemons cannot make models resident on the same GPU.
package devicelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/owner"
	"github.com/contenox/contenox/libtransport"
)

const (
	// EnvLeaseDir overrides the directory used for fixed-location device leases.
	// It exists for tests and for operators with non-standard cache locations.
	EnvLeaseDir = "CONTENOX_MODELD_DEVICE_LEASE_DIR"

	defaultTTL = 30 * time.Second
)

// Service wraps a transport.Service with accelerator-device ownership.
type Service struct {
	next transport.Service
	ttl  time.Duration
	dir  string
	meta map[string]string
}

type Option func(*Service)

// WithTTL sets the device lease TTL. Owners renew until the session closes.
func WithTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithLeaseDir sets the directory where device lease files are stored.
func WithLeaseDir(dir string) Option {
	return func(s *Service) { s.dir = dir }
}

// WithMeta attaches holder metadata to the lease record.
func WithMeta(meta map[string]string) Option {
	return func(s *Service) {
		if len(meta) == 0 {
			return
		}
		s.meta = map[string]string{}
		for k, v := range meta {
			if k != "" && v != "" {
				s.meta[k] = v
			}
		}
	}
}

// New returns next wrapped with accelerator-device leases.
func New(next transport.Service, opts ...Option) *Service {
	s := &Service{next: next, ttl: defaultTTL}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

var _ transport.Service = (*Service)(nil)

func (s *Service) OpenSession(ctx context.Context, req transport.OpenSessionRequest) (transport.Session, error) {
	hold, err := s.acquireForOpen(ctx, req)
	if err != nil {
		return nil, err
	}
	sess, err := s.next.OpenSession(ctx, req)
	if err != nil {
		if hold != nil {
			_ = hold.release()
		}
		return nil, err
	}
	if hold == nil {
		return sess, nil
	}
	return &session{Session: sess, hold: hold}, nil
}

func (s *Service) Describe(ctx context.Context, req transport.OpenSessionRequest) (transport.ModelInfo, error) {
	return s.next.Describe(ctx, req)
}

func (s *Service) Embed(ctx context.Context, req transport.EmbedRequest) (transport.EmbedResult, error) {
	hold, err := s.acquireForOpen(ctx, transport.OpenSessionRequest{
		Fence:     req.Fence,
		ModelName: req.ModelName,
		Type:      req.Type,
		Digest:    req.Digest,
		Path:      req.Path,
		Config:    req.Config,
	})
	if err != nil {
		return transport.EmbedResult{}, err
	}
	if hold != nil {
		defer hold.release()
	}
	return s.next.Embed(ctx, req)
}

func (s *Service) acquireForOpen(ctx context.Context, req transport.OpenSessionRequest) (*hold, error) {
	if s.next == nil {
		return nil, errors.New("devicelease: nil transport service")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	info, err := s.next.Describe(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return s.acquire(info)
}

func (s *Service) acquire(info transport.ModelInfo) (*hold, error) {
	target, ok, err := targetFromInfo(info, s.dir)
	if err != nil || !ok {
		return nil, err
	}
	meta := map[string]string{
		"lease_kind":  "device",
		"device_kind": target.kind,
		"device_id":   target.id,
	}
	for k, v := range s.meta {
		if k != "" && v != "" {
			meta[k] = v
		}
	}
	o, err := owner.Join(context.Background(), owner.Config{
		LeasePath: target.path,
		TTL:       s.ttl,
		Meta:      meta,
	})
	if err != nil {
		return nil, err
	}
	if !o.IsOwner() {
		h := o.Holder()
		return nil, fmt.Errorf("%w: accelerator device %s/%s is already owned by modeld pid=%d host=%s data_root=%q backend=%q lease=%q expires=%s",
			transport.ErrDeviceBusy,
			target.kind,
			target.id,
			h.PID,
			h.Host,
			h.Meta["data_root"],
			h.Meta["backend"],
			target.path,
			h.ExpiresAt().Format(time.RFC3339),
		)
	}
	return &hold{owner: o}, nil
}

type hold struct {
	owner *owner.Owner
}

func (h *hold) release() error {
	if h == nil || h.owner == nil {
		return nil
	}
	return h.owner.Release()
}

type session struct {
	transport.Session
	hold     *hold
	close    sync.Once
	closeErr error
}

func (s *session) Close() error {
	s.close.Do(func() {
		s.closeErr = errors.Join(s.Session.Close(), s.hold.release())
	})
	return s.closeErr
}

type target struct {
	kind string
	id   string
	path string
}

func targetFromInfo(info transport.ModelInfo, dir string) (target, bool, error) {
	st := capacity.DeviceSnapshot{Kind: info.DeviceKind}
	if !st.IsAccelerator() {
		return target{}, false, nil
	}
	kind := strings.ToLower(strings.TrimSpace(info.DeviceKind))
	id := strings.TrimSpace(info.DeviceID)
	if id == "" {
		id = "unknown"
	}
	root, err := leaseDir(dir)
	if err != nil {
		return target{}, false, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return target{}, false, fmt.Errorf("device lease dir %q: %w", root, err)
	}
	return target{
		kind: kind,
		id:   id,
		path: filepath.Join(root, leaseFileName(kind, id)),
	}, true, nil
}

func leaseDir(override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		return override, nil
	}
	if override = strings.TrimSpace(os.Getenv(EnvLeaseDir)); override != "" {
		return override, nil
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "contenox", "modeld", "devices"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve device lease directory: %w", err)
	}
	return filepath.Join(home, ".cache", "contenox", "modeld", "devices"), nil
}

func leaseFileName(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return fmt.Sprintf("%s-%s-%s.lease", slug(kind), slug(id), hex.EncodeToString(sum[:])[:12])
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 48 {
		return out[:48]
	}
	return out
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
