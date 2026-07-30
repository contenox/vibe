package transport

import (
	"context"
	"io"
)

// NodeModel is one model as observed on a node's models directory.
type NodeModel struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Digest    string `json:"digest,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// ContextLength is the model's trained context ceiling, read from file
	// metadata only (GGUF header / OpenVINO config.json) — no device query, no
	// capacity planner. 0 means unknown (header parse failed or was skipped).
	ContextLength int `json:"context_length,omitempty"`
}

// NodeDiskStats reports free/used/total bytes on the filesystem backing a
// node's models directory.
type NodeDiskStats struct {
	FreeBytes  int64 `json:"free_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// PushFormat identifies how a PushModel byte stream is laid out on disk.
type PushFormat string

const (
	// PushFormatFile is a single-file model (llama GGUF).
	PushFormatFile PushFormat = "file"
	// PushFormatTar is a directory model sent as an uncompressed tar stream:
	// an OpenVINO IR bundle, or a llama vision model shipping model.gguf plus
	// its mmproj.gguf projector as one atomic install.
	PushFormatTar PushFormat = "tar"
)

// PushManifest describes an incoming model push. Digest is the sha256 hex of
// the raw byte stream exactly as sent (the tar stream itself for
// PushFormatTar, not a hash of the unpacked contents).
type PushManifest struct {
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Digest     string     `json:"digest,omitempty"`
	TotalBytes int64      `json:"total_bytes,omitempty"`
	Format     PushFormat `json:"format"`
}

// PushResult reports what PushModel actually did.
type PushResult struct {
	// AlreadyPresent means a model with this name and a matching digest was
	// already installed on the node; the pushed bytes were verified then
	// discarded rather than replacing it.
	AlreadyPresent bool  `json:"already_present,omitempty"`
	BytesWritten   int64 `json:"bytes_written,omitempty"`
}

// NodeAdmin is implemented by modeld services that expose model-store
// management for a node: inventory, removal, disk capacity, and receiving
// model bytes the runtime pushes. It is an optional extension of Service, the
// same way ModelController is — a caller type-asserts for it.
//
// Model distribution is runtime-push-only by design: a node never fetches a
// model from an external source (HuggingFace, etc.); the runtime is the sole
// source of model bytes, and NodeAdmin's job is a correct, idempotent local
// sink for them.
type NodeAdmin interface {
	// ListModels enumerates every model present in the node's models
	// directory.
	ListModels(ctx context.Context) ([]NodeModel, error)

	// RemoveModel deletes a model from the node's models directory.
	RemoveModel(ctx context.Context, name string) error

	// DiskStats reports free/used/total bytes on the filesystem backing the
	// node's models directory, so a caller can check a push will fit before
	// sending gigabytes of model weights.
	DiskStats(ctx context.Context) (NodeDiskStats, error)

	// ReceiveModel consumes r (the raw byte stream described by manifest),
	// verifies it against manifest.Digest, and atomically installs it. It is
	// the sink side of the PushModel wire RPC — the wire layer owns framing
	// and chunking; this method only sees a plain io.Reader.
	ReceiveModel(ctx context.Context, manifest PushManifest, r io.Reader) (PushResult, error)
}
