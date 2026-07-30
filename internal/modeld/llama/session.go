// Package llama defines the modeld-side llama backend contract: persistent
// inference sessions keep a stable prefix's KV hot and re-prefill only the
// changed suffix.
//
// This package defines the backend-neutral session contract. Backend adapters
// implement it; product code talks to Session, not llama.cpp internals.
// Snapshot/restore is part of the same contract for durability and branching.
// The main generation path is EnsurePrefix -> PrefillSuffix -> Decode on a live
// session.
package llama

import (
	"context"
	"errors"
	"fmt"

	"github.com/contenox/contenox/libtransport"
)

type Config = transport.Config

// AdapterSpec identifies one LoRA adapter to apply to a session: a GGUF adapter
// file (Path) applied at Scale, plus Name/Digest carried for cache identity and
// diagnostics. Applying an adapter does not modify the base model weights, but it
// changes model behavior, so adapter identity must be part of every session and
// manifest cache key (see docs/development/blueprints/modeld/lora-adapters.md). It mirrors the
// transport-level adapter handle without importing the wire shape here.
type AdapterSpec struct {
	Name   string
	Path   string
	Digest string
	Scale  float32
}

type PrefixInput = transport.PrefixInput
type SuffixInput = transport.SuffixInput
type PrefixStatus = transport.PrefixStatus
type SuffixStatus = transport.SuffixStatus
type DecodeConfig = transport.DecodeConfig
type ToolCall = transport.ToolCall
type StreamChunk = transport.StreamChunk
type ContextReport = transport.ContextReport
type SessionSnapshot = transport.SessionSnapshot
type ColdKVBlock = transport.ColdKVBlock
type Session = transport.Session

// SessionFactory creates a backend session for a model with explicit config and
// any LoRA adapters to apply to the session. Empty adapters = the base model.
type SessionFactory func(modelPath string, cfg Config, adapters []AdapterSpec) (Session, error)

var sessionFactory SessionFactory

// SetSessionFactory registers the backend that creates sessions. The llama.cpp
// adapter (./llamasession) calls this from its init when built with the
// 'llamanode' tag, so the provider never imports the CGo package directly (no
// import cycle, default build stays CGo-free).
func SetSessionFactory(f SessionFactory) { sessionFactory = f }

// SessionAvailable reports whether a session backend is compiled into this build.
func SessionAvailable() bool { return sessionFactory != nil }

// newSession creates a session through the registered backend, applying any LoRA
// adapters to the session context.
func newSession(modelPath string, cfg Config, adapters []AdapterSpec) (Session, error) {
	if sessionFactory == nil {
		return nil, fmt.Errorf("%w: build modeld with -tags 'llamanode llamacpp_direct'", ErrSessionUnavailable)
	}
	return sessionFactory(modelPath, cfg, adapters)
}

// EmbedFunc computes a single embedding via the native backend. The llama.cpp
// adapter registers one from its init when built with the 'llamanode' tag.
type EmbedFunc func(ctx context.Context, modelPath string, cfg Config, input string) ([]float64, error)

var embedFunc EmbedFunc

// SetEmbedFunc registers the native embedding backend.
func SetEmbedFunc(f EmbedFunc) { embedFunc = f }

// EmbedAvailable reports whether an embedding backend is compiled into this build.
func EmbedAvailable() bool { return embedFunc != nil }

// newEmbed computes an embedding through the registered backend.
func newEmbed(ctx context.Context, modelPath string, cfg Config, input string) ([]float64, error) {
	if embedFunc == nil {
		return nil, fmt.Errorf("%w: build modeld with -tags 'llamanode llamacpp_direct'", ErrSessionUnavailable)
	}
	return embedFunc(ctx, modelPath, cfg, input)
}

var (
	// ErrSessionUnavailable means no native llama backend was compiled into
	// this binary.
	ErrSessionUnavailable = errors.New("llama: session backend unavailable")
	// ErrSessionClosed means the caller used a closed persistent session.
	//
	// ErrSessionClosed, ErrContextOverflow, and ErrUnsupportedFeature alias the
	// transport sentinels (as manifest.go already aliases contextasm.ErrManifestMismatch)
	// so the daemon's session errors survive the modeld wire boundary: the gRPC
	// error map (runtime/transport/grpc/errors.go) keys on the transport.Err*
	// values, and an error that is not Is-compatible with them is downgraded to
	// codes.Internal. The typed *ContextOverflowError / *UnsupportedFeatureError
	// still wrap these, so errors.As keeps returning their structured fields.
	ErrSessionClosed = transport.ErrSessionClosed
	// ErrContextOverflow means a prefix, suffix, or decode would exceed NumCtx.
	ErrContextOverflow = transport.ErrContextOverflow
	// ErrUnsupportedFeature marks explicit product-surface gaps such as tools.
	ErrUnsupportedFeature = transport.ErrUnsupportedFeature
	// ErrSessionFatal means the backend marked the session unusable and callers
	// must evict it instead of trying to reuse resident KV. It aliases the
	// transport sentinel so the existing decode/restore emissions classify over
	// the modeld wire boundary instead of degrading to codes.Internal.
	ErrSessionFatal = transport.ErrSessionFatal
)

type ContextOverflowError = transport.ContextOverflowError

func NewContextOverflowError(stage string, resident, additional, numCtx int) error {
	return transport.NewContextOverflowError(stage, resident, additional, numCtx)
}

// UnsupportedFeatureError describes a deliberately unsupported surface.
type UnsupportedFeatureError struct {
	Feature string
}

func (e *UnsupportedFeatureError) Error() string {
	if e.Feature == "" {
		return ErrUnsupportedFeature.Error()
	}
	return fmt.Sprintf("%s: %s", ErrUnsupportedFeature, e.Feature)
}

func (e *UnsupportedFeatureError) Is(target error) bool {
	return target == ErrUnsupportedFeature
}

func NewUnsupportedFeatureError(feature string) error {
	return &UnsupportedFeatureError{Feature: feature}
}
