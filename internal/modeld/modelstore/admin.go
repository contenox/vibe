package modelstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/openvino"
	"github.com/contenox/contenox/internal/archiveutil"
	"github.com/shirou/gopsutil/v4/disk"
)

// NodeModel is one model as observed on a node's models directory.
type NodeModel struct {
	Name      string
	Type      string // "llama" | "openvino"
	Digest    string // sha256 hex; empty for openvino (see Resolve doc)
	SizeBytes int64
	// ContextLength is the model's trained context ceiling, from a header-only
	// metadata parse (no device query, no capacity planner) — 0 if that parse
	// failed or was skipped; see ListModels.
	ContextLength int
}

// DiskStats reports free/used space on the filesystem backing the models
// directory, so the runtime can decide whether a push would fit before
// sending gigabytes of model weights.
type DiskStats struct {
	FreeBytes  int64
	UsedBytes  int64
	TotalBytes int64
}

// PushFormat identifies how a PushModel byte stream is laid out.
type PushFormat string

const (
	// PushFormatFile is a single-file model (llama GGUF), written as
	// <dir>/<name>/model.gguf.
	PushFormatFile PushFormat = "file"
	// PushFormatTar is a directory model sent as an uncompressed tar stream and
	// unpacked into <dir>/<name>/: an OpenVINO IR bundle, or a llama vision
	// model shipping model.gguf plus its mmproj.gguf projector (see
	// ResolveMMProj) — the tar keeps the two files one atomic install.
	PushFormatTar PushFormat = "tar"
)

// PushManifest describes an incoming model push. Digest is the sha256 of the
// raw byte stream exactly as sent (the tar stream itself for PushFormatTar,
// not a hash of the unpacked contents) — the receiver verifies it as bytes
// arrive, before touching the models directory.
type PushManifest struct {
	Name       string
	Type       string // "llama" | "openvino"
	Digest     string
	TotalBytes int64
	Format     PushFormat
}

// PushResult reports what ReceiveModel actually did.
type PushResult struct {
	// AlreadyPresent means a model with this name and a matching digest was
	// already installed; the newly received bytes were verified then
	// discarded rather than replacing it.
	AlreadyPresent bool
	BytesWritten   int64
}

const stagingPrefix = ".staging-"

// Admin implements node-side model store management: listing, removal, disk
// stats, and receiving pushed model blobs. Per the runtime's model
// distribution design, a node never fetches from an external source — the
// runtime is the only source of model bytes, and Admin is purely a local
// sink/inventory over the models directory.
type Admin struct {
	dir string

	mu       sync.Mutex
	inFlight map[string]struct{}
}

// NewAdmin returns an Admin operating on dir (a resolved models directory,
// see Dir).
func NewAdmin(dir string) *Admin {
	return &Admin{dir: dir, inFlight: map[string]struct{}{}}
}

func (a *Admin) claim(name string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.inFlight[name]; busy {
		return nil, fmt.Errorf("modelstore: push already in progress for %q", name)
	}
	a.inFlight[name] = struct{}{}
	return func() {
		a.mu.Lock()
		delete(a.inFlight, name)
		a.mu.Unlock()
	}, nil
}

// ListModels enumerates every model in the models directory.
func (a *Admin) ListModels(_ context.Context) ([]NodeModel, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("modelstore: read models dir: %w", err)
	}
	var out []NodeModel
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		name := e.Name()
		if path, err := Resolve(a.dir, name, "llama", ""); err == nil {
			m := NodeModel{Name: name, Type: "llama"}
			if info, statErr := os.Stat(path); statErr == nil {
				m.SizeBytes = info.Size()
			}
			// SizeBytes is the install footprint, so a vision model's projector
			// counts too; Digest stays the model.gguf content digest (the cache
			// identity — see ResolveMMProj).
			if mmproj := ResolveMMProj(path); mmproj != "" {
				if info, statErr := os.Stat(mmproj); statErr == nil {
					m.SizeBytes += info.Size()
				}
			}
			if digest, digestErr := FileDigest(path); digestErr == nil {
				m.Digest = digest
			}
			// Header-only parse (no tensor data, no device query); a failure
			// here must not hide this model from the inventory — it just
			// leaves ContextLength at 0 ("unknown").
			if cl, clErr := llama.ContextLength(path); clErr == nil {
				m.ContextLength = cl
			}
			out = append(out, m)
			continue
		}
		if dir, err := Resolve(a.dir, name, "openvino", ""); err == nil {
			size, _ := dirSize(dir)
			m := NodeModel{Name: name, Type: "openvino", SizeBytes: size}
			if cl, clErr := openvino.ContextLength(dir); clErr == nil {
				m.ContextLength = cl
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// RemoveModel deletes a model's directory. ErrModelNotFound if it does not
// exist.
func (a *Admin) RemoveModel(_ context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("modelstore: empty model name")
	}
	modelDir := filepath.Join(a.dir, name)
	if _, err := os.Stat(modelDir); err != nil {
		if os.IsNotExist(err) {
			return ErrModelNotFound
		}
		return fmt.Errorf("modelstore: stat %s: %w", modelDir, err)
	}
	if err := os.RemoveAll(modelDir); err != nil {
		return fmt.Errorf("modelstore: remove %s: %w", modelDir, err)
	}
	return nil
}

// DiskStats reports free/used/total bytes on the filesystem backing the
// models directory.
func (a *Admin) DiskStats(ctx context.Context) (DiskStats, error) {
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return DiskStats{}, fmt.Errorf("modelstore: ensure models dir: %w", err)
	}
	u, err := disk.UsageWithContext(ctx, a.dir)
	if err != nil {
		return DiskStats{}, fmt.Errorf("modelstore: disk usage: %w", err)
	}
	return DiskStats{
		FreeBytes:  int64(u.Free),
		UsedBytes:  int64(u.Used),
		TotalBytes: int64(u.Total),
	}, nil
}

// ReceiveModel is the sink side of PushModel: it consumes r (the raw byte
// stream described by manifest), verifies it against manifest.Digest, and
// atomically installs it as the named model. Idempotent: an existing model
// with a matching digest is kept and the newly received bytes discarded.
//
// Deciding whether to push at all — skipping when the model is already
// present with a matching digest — is the caller's job (the runtime's
// reconcile loop checks ListModels first, since it can do that without
// transferring any bytes). ReceiveModel always accepts and verifies a full
// stream so there is exactly one, always-correct write path; it is not an
// optimization for the already-present case.
//
// At most one push per model name may be in flight at a time; a concurrent
// second push for the same name is rejected immediately.
func (a *Admin) ReceiveModel(_ context.Context, manifest PushManifest, r io.Reader) (PushResult, error) {
	if manifest.Name == "" {
		return PushResult{}, fmt.Errorf("modelstore: push manifest missing model name")
	}
	if manifest.Type != "llama" && manifest.Type != "openvino" {
		return PushResult{}, fmt.Errorf("%w: %q", ErrUnsupportedType, manifest.Type)
	}

	release, err := a.claim(manifest.Name)
	if err != nil {
		return PushResult{}, err
	}
	defer release()

	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return PushResult{}, fmt.Errorf("modelstore: ensure models dir: %w", err)
	}
	staging, err := os.MkdirTemp(a.dir, stagingPrefix+manifest.Name+"-*")
	if err != nil {
		return PushResult{}, fmt.Errorf("modelstore: create staging dir: %w", err)
	}
	defer os.RemoveAll(staging) // no-op once renamed into place below

	hasher := sha256.New()
	tee := io.TeeReader(r, hasher)

	var written int64
	switch manifest.Format {
	case PushFormatFile:
		written, err = writeStagedFile(staging, tee)
	case PushFormatTar:
		written, err = writeStagedTar(staging, tee)
	default:
		return PushResult{}, fmt.Errorf("modelstore: unsupported push format %q", manifest.Format)
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("modelstore: write staged content: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if manifest.Digest != "" && got != manifest.Digest {
		return PushResult{}, fmt.Errorf("%w: model %s want=%s got=%s", ErrDigestMismatch, manifest.Name, manifest.Digest, got)
	}

	if _, err := Resolve(a.dir, manifest.Name, manifest.Type, got); err == nil {
		return PushResult{AlreadyPresent: true, BytesWritten: written}, nil
	}

	modelDir := filepath.Join(a.dir, manifest.Name)
	if err := os.RemoveAll(modelDir); err != nil {
		return PushResult{}, fmt.Errorf("modelstore: clear existing model dir: %w", err)
	}
	if err := os.Rename(staging, modelDir); err != nil {
		return PushResult{}, fmt.Errorf("modelstore: install model %s: %w", manifest.Name, err)
	}
	return PushResult{BytesWritten: written}, nil
}

// writeStagedFile writes r as staging/model.gguf, matching the layout Resolve
// expects for a llama model.
func writeStagedFile(staging string, r io.Reader) (int64, error) {
	target := filepath.Join(staging, "model.gguf")
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// writeStagedTar unpacks r as a tar stream directly into staging, matching
// the layout Resolve expects for an OpenVINO IR model (entrypoint files at
// the directory root). countingReader tracks the raw bytes consumed so the
// caller's digest and BytesWritten reflect the wire stream, not the unpacked
// size.
func writeStagedTar(staging string, r io.Reader) (int64, error) {
	cr := &countingReader{r: r}
	if err := archiveutil.ExtractTar(cr, staging); err != nil {
		return cr.n, err
	}
	return cr.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// dirSize sums the size of every regular file under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
