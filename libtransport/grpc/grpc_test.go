package grpc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/slot"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
	transportgrpc "github.com/contenox/contenox/libtransport/grpc"
)

// startServer serves a MemoryService over gRPC on a loopback port and returns a
// fenced client dialed at the given expectedOwner.
func startServer(t *testing.T, ownerFence, expectedOwner string) *transportgrpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := transport.NewMemoryService(transport.WithOwnerFence(ownerFence))
	return startServerWithService(t, svc, lis, ownerFence, expectedOwner)
}

func startServerWithService(t *testing.T, svc transport.Service, lis net.Listener, ownerFence, expectedOwner string) *transportgrpc.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = transportgrpc.Serve(ctx, lis, svc, ownerFence, "test") }()

	client, err := transportgrpc.DialLeader(lis.Addr().String(), expectedOwner)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(); cancel() })
	return client
}

type embeddingService struct {
	*transport.MemoryService

	mu   sync.Mutex
	last transport.EmbedRequest
}

func (s *embeddingService) Embed(ctx context.Context, req transport.EmbedRequest) (transport.EmbedResult, error) {
	if _, err := s.MemoryService.Embed(ctx, req); errors.Is(err, transport.ErrStaleFence) {
		return transport.EmbedResult{}, err
	}
	s.mu.Lock()
	s.last = req
	s.mu.Unlock()
	return transport.EmbedResult{Vector: []float32{1, 2.5, -3}}, nil
}

func (s *embeddingService) lastRequest() transport.EmbedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

type decodeRecordingService struct {
	*transport.MemoryService
	sess transport.Session
}

func (s *decodeRecordingService) OpenSession(context.Context, transport.OpenSessionRequest) (transport.Session, error) {
	return s.sess, nil
}

type canceledOpenService struct {
	*transport.MemoryService
}

func (s *canceledOpenService) OpenSession(context.Context, transport.OpenSessionRequest) (transport.Session, error) {
	return nil, context.Canceled
}

type describeService struct {
	*transport.MemoryService
	info transport.ModelInfo
}

func (s *describeService) Describe(context.Context, transport.OpenSessionRequest) (transport.ModelInfo, error) {
	return s.info, nil
}

type streamErrorSession struct {
	decodeRecordingSession
	err error
}

func (s *streamErrorSession) Decode(context.Context, transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	ch := make(chan transport.StreamChunk, 1)
	ch <- transport.StreamChunk{Error: s.err}
	close(ch)
	return ch, nil
}

type overflowPrefixSession struct {
	decodeRecordingSession
	err error
}

func (s *overflowPrefixSession) EnsurePrefix(context.Context, transport.PrefixInput) (transport.PrefixStatus, error) {
	return transport.PrefixStatus{}, s.err
}

type decodeRecordingSession struct {
	mu     sync.Mutex
	config transport.DecodeConfig
}

type largeSnapshotSession struct {
	decodeRecordingSession

	snap transport.SessionSnapshot

	mu         sync.Mutex
	restoredID string
}

func transportToolCall(id, name, arguments string) transport.ToolCall {
	var tc transport.ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = arguments
	return tc
}

func (s *decodeRecordingSession) EnsurePrefix(context.Context, transport.PrefixInput) (transport.PrefixStatus, error) {
	return transport.PrefixStatus{}, nil
}

func (s *decodeRecordingSession) PrefillSuffix(context.Context, transport.SuffixInput) (transport.SuffixStatus, error) {
	return transport.SuffixStatus{}, nil
}

func (s *decodeRecordingSession) Decode(_ context.Context, cfg transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	s.mu.Lock()
	s.config = cfg
	s.mu.Unlock()
	ch := make(chan transport.StreamChunk, 2)
	ch <- transport.StreamChunk{Thinking: "think"}
	ch <- transport.StreamChunk{Text: "answer", ToolCalls: []transport.ToolCall{transportToolCall("call_1", "lookup", `{"q":"x"}`)}}
	close(ch)
	return ch, nil
}

func (s *decodeRecordingSession) ExplainContext() transport.ContextReport {
	return transport.ContextReport{}
}
func (s *decodeRecordingSession) Snapshot(context.Context) (transport.SessionSnapshot, error) {
	return transport.SessionSnapshot{}, nil
}
func (s *decodeRecordingSession) Restore(context.Context, transport.SessionSnapshot) error {
	return nil
}
func (s *decodeRecordingSession) Close() error { return nil }

func (s *largeSnapshotSession) Snapshot(context.Context) (transport.SessionSnapshot, error) {
	return s.snap, nil
}

func (s *largeSnapshotSession) Restore(_ context.Context, snap transport.SessionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoredID = snap.StateID
	return nil
}

func (s *largeSnapshotSession) restoredStateID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoredID
}

func (s *decodeRecordingSession) lastConfig() transport.DecodeConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

func manifest(stable string) contextasm.ContextManifest {
	return contextasm.ContextManifest{
		Backend:              "mem",
		ModelDigest:          "d1",
		PromptFormat:         "f1",
		PromptTemplateDigest: "t1",
		RuntimeDigest:        "r1",
		StableByteHash:       contextasm.HashString(stable),
	}
}

func TestDecodeMetadataRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rec := &decodeRecordingSession{}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess:          rec,
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	ch, err := sess.Decode(context.Background(), transport.DecodeConfig{
		MaxTokens:       4,
		ParserProtocols: []string{"openvino:deepseek_r1_reasoning_incremental_parser"},
		ReasoningFormat: "deepseek",
		StructuredOutput: transport.StructuredOutputConfig{
			Protocol: "openvino:json_schema_tool_calls",
			Payload:  `{"type":"object"}`,
			ToolName: "echo.echo",
		},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var text, thinking string
	var calls []transport.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode chunk error: %v", chunk.Error)
		}
		text += chunk.Text
		thinking += chunk.Thinking
		calls = append(calls, chunk.ToolCalls...)
	}
	if text != "answer" || thinking != "think" {
		t.Fatalf("stream text/thinking = %q/%q", text, thinking)
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("stream tool calls = %+v", calls)
	}
	cfg := rec.lastConfig()
	if cfg.MaxTokens != 4 || len(cfg.ParserProtocols) != 1 || cfg.ParserProtocols[0] != "openvino:deepseek_r1_reasoning_incremental_parser" ||
		cfg.ReasoningFormat != "deepseek" ||
		cfg.StructuredOutput.Protocol != "openvino:json_schema_tool_calls" ||
		cfg.StructuredOutput.Payload != `{"type":"object"}` ||
		cfg.StructuredOutput.ToolName != "echo.echo" {
		t.Fatalf("decode config over wire = %+v", cfg)
	}
}

func TestRoundTripContractOverWire(t *testing.T) {
	client := startServer(t, "owner-1", "owner-1")
	ctx := context.Background()

	sess, err := client.OpenSession(ctx, transport.OpenSessionRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "m",
		Path:      "m",
		Config:    transport.Config{NumCtx: 100},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	m := manifest("hello")
	cold, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "hello", Manifest: m})
	if err != nil {
		t.Fatalf("EnsurePrefix cold: %v", err)
	}
	if cold.ReusedTokens != 0 || cold.PrefilledTokens != 5 || cold.PrefixTokens != 5 {
		t.Fatalf("cold status over wire = %+v", cold)
	}
	warm, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "hello", Manifest: m})
	if err != nil {
		t.Fatalf("EnsurePrefix warm: %v", err)
	}
	if warm.ReusedTokens != 5 || warm.PrefilledTokens != 0 {
		t.Fatalf("warm status over wire = %+v", warm)
	}

	if _, err := sess.PrefillSuffix(ctx, transport.SuffixInput{Text: " world", Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	ch, err := sess.Decode(ctx, transport.DecodeConfig{MaxTokens: 3})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var n int
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("decode streamed %d chunks, want 3", n)
	}

	report := sess.ExplainContext()
	if report.ResidentTokens != 11 { // "hello"(5) + " world"(6)
		t.Fatalf("ExplainContext resident = %d, want 11", report.ResidentTokens)
	}
	snap, err := sess.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.ResidentTokens != 11 || snap.PrefixTokens != 5 || snap.Manifest.Digest() != m.Digest() {
		t.Fatalf("snapshot over wire = %+v, want resident=11 prefix=5 manifest digest %q", snap, m.Digest())
	}
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "other", Manifest: manifest("other")}); err != nil {
		t.Fatalf("EnsurePrefix other: %v", err)
	}
	if err := sess.Restore(ctx, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	report = sess.ExplainContext()
	if report.ResidentTokens != 11 || report.ManifestDigest != m.Digest() {
		t.Fatalf("ExplainContext after restore = %+v, want restored snapshot", report)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After close the handle is gone: the sentinel must survive the wire.
	if _, err := sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "x", Manifest: manifest("x")}); !errors.Is(err, transport.ErrSessionClosed) {
		t.Fatalf("EnsurePrefix after close = %v, want ErrSessionClosed", err)
	}
}

func TestSnapshotStateIDRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The raw State blob is deliberately excluded from the wire (json:"-"); the
	// slot layer indirects it through a StateID-keyed blob on the daemon's disk.
	// The transport must carry the StateID and metadata, and must drop State so a
	// multi-megabyte blob never travels over gRPC.
	const stateID = "deadbeefcafef00d"
	large := &largeSnapshotSession{
		snap: transport.SessionSnapshot{
			State:          bytes.Repeat([]byte{0x7a}, 5<<20),
			StateID:        stateID,
			ResidentTokens: 42,
			PrefixTokens:   21,
			NumCtx:         128,
			Manifest:       manifest("large"),
		},
	}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess:          large,
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	snap, err := sess.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.StateID != stateID {
		t.Fatalf("StateID not preserved over wire: got %q want %q", snap.StateID, stateID)
	}
	if len(snap.State) != 0 {
		t.Fatalf("raw State must not cross the wire: got %d bytes", len(snap.State))
	}
	if snap.ResidentTokens != 42 || snap.PrefixTokens != 21 || snap.NumCtx != 128 {
		t.Fatalf("snapshot metadata not preserved over wire: %+v", snap)
	}
	if err := sess.Restore(context.Background(), snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := large.restoredStateID(); got != stateID {
		t.Fatalf("restored StateID = %q, want %q", got, stateID)
	}
}

func TestStaleFenceRejectedOverWire(t *testing.T) {
	client := startServer(t, "owner-1", "wrong-owner")
	_, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{ModelName: "m", Path: "m"})
	if !errors.Is(err, transport.ErrStaleFence) {
		t.Fatalf("OpenSession with wrong owner = %v, want ErrStaleFence", err)
	}
}

func TestContextOverflowSentinelOverWire(t *testing.T) {
	client := startServer(t, "owner-1", "owner-1")
	ctx := context.Background()
	sess, err := client.OpenSession(ctx, transport.OpenSessionRequest{
		Fence:  transport.Fence{OwnerInstanceID: "owner-1"},
		Config: transport.Config{NumCtx: 4},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	_, err = sess.EnsurePrefix(ctx, transport.PrefixInput{Text: "too many tokens", Manifest: manifest("too many tokens")})
	if !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("EnsurePrefix overflow = %v, want ErrContextOverflow", err)
	}
}

func TestContextOverflowDetailsOverUnaryWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess: &overflowPrefixSession{
			err: transport.NewContextOverflowError("prefix", 2, 5, 4),
		},
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")

	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	_, err = sess.EnsurePrefix(context.Background(), transport.PrefixInput{Text: "too many", Manifest: manifest("too many")})
	if !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("EnsurePrefix overflow = %v, want ErrContextOverflow", err)
	}
	var overflow *transport.ContextOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("EnsurePrefix overflow = %T %[1]v, want typed ContextOverflowError", err)
	}
	if overflow.Stage != "prefix" || overflow.ResidentTokens != 2 || overflow.AdditionalTokens != 5 || overflow.NumCtx != 4 {
		t.Fatalf("overflow detail = %+v", overflow)
	}
}

func TestContextOverflowDetailsOverDecodeStream(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess:          &streamErrorSession{err: transport.NewContextOverflowError("decode", 3, 1, 4)},
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")

	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	ch, err := sess.Decode(context.Background(), transport.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	chunk, ok := <-ch
	if !ok {
		t.Fatal("decode stream closed without chunk")
	}
	if !errors.Is(chunk.Error, transport.ErrContextOverflow) {
		t.Fatalf("chunk error = %v, want ErrContextOverflow", chunk.Error)
	}
	var overflow *transport.ContextOverflowError
	if !errors.As(chunk.Error, &overflow) {
		t.Fatalf("chunk error = %T %[1]v, want typed ContextOverflowError", chunk.Error)
	}
	if overflow.Stage != "decode" || overflow.ResidentTokens != 3 || overflow.AdditionalTokens != 1 || overflow.NumCtx != 4 {
		t.Fatalf("overflow detail = %+v", overflow)
	}
}

func TestContextCanceledSentinelOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &canceledOpenService{MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1"))}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")

	_, err = client.OpenSession(context.Background(), transport.OpenSessionRequest{Fence: transport.Fence{OwnerInstanceID: "owner-1"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenSession canceled over wire = %v, want context.Canceled", err)
	}
}

func TestDescribeContextBudgetRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	want := transport.ModelInfo{
		ModelMaxContext:                     32768,
		EffectiveContext:                    8192,
		MemoryContextTokens:                 12000,
		HotContextTokens:                    8192,
		PlannerEffectiveContext:             8192,
		KVBytesPerToken:                     2048,
		FreeBytes:                           64 << 20,
		WeightsBytes:                        4 << 20,
		UsableBytes:                         32 << 20,
		RequiredBytes:                       20 << 20,
		Clamped:                             true,
		Reason:                              "request_exceeds_memory_budget",
		DeviceKind:                          "gpu",
		DeviceID:                            "GPU.0",
		SparseAttention:                     true,
		SlidingWindowAttentionTokens:        4096,
		SupportsVision:                      true,
		VisionTokensPerImage:                256,
		ChatTemplateFormat:                  "peg-native",
		ChatTemplateThinkingStartTag:        "<think>",
		ChatTemplateReasoningFormat:         "auto",
		ChatTemplateSupportsToolCalls:       true,
		ChatTemplateSupportsThinking:        true,
		ChatTemplateSupportsReasoningEffort: true,
	}
	svc := &describeService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		info:          want,
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")

	got, err := client.Describe(context.Background(), transport.OpenSessionRequest{
		Fence:  transport.Fence{OwnerInstanceID: "owner-1"},
		Config: transport.Config{NumCtx: 8192},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.ModelMaxContext != want.ModelMaxContext ||
		got.EffectiveContext != want.EffectiveContext ||
		got.MemoryContextTokens != want.MemoryContextTokens ||
		got.HotContextTokens != want.HotContextTokens ||
		got.PlannerEffectiveContext != want.PlannerEffectiveContext ||
		got.KVBytesPerToken != want.KVBytesPerToken ||
		got.RequiredBytes != want.RequiredBytes ||
		got.Reason != want.Reason ||
		got.DeviceKind != want.DeviceKind ||
		got.DeviceID != want.DeviceID ||
		got.SparseAttention != want.SparseAttention ||
		got.SlidingWindowAttentionTokens != want.SlidingWindowAttentionTokens ||
		got.SupportsVision != want.SupportsVision ||
		got.VisionTokensPerImage != want.VisionTokensPerImage ||
		got.ChatTemplateFormat != want.ChatTemplateFormat ||
		got.ChatTemplateThinkingStartTag != want.ChatTemplateThinkingStartTag ||
		got.ChatTemplateReasoningFormat != want.ChatTemplateReasoningFormat ||
		got.ChatTemplateSupportsToolCalls != want.ChatTemplateSupportsToolCalls ||
		got.ChatTemplateSupportsThinking != want.ChatTemplateSupportsThinking ||
		got.ChatTemplateSupportsReasoningEffort != want.ChatTemplateSupportsReasoningEffort {
		t.Fatalf("Describe over wire = %+v, want %+v", got, want)
	}
}

func TestEmbedRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &embeddingService{MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1"))}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")

	res, err := client.Embed(context.Background(), transport.EmbedRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "embedder",
		Type:      "openvino",
		Digest:    "sha256:test",
		Path:      "/models/embedder",
		Text:      "query text",
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(res.Vector) != 3 || res.Vector[0] != 1 || res.Vector[1] != 2.5 || res.Vector[2] != -3 {
		t.Fatalf("Embed vector = %+v", res.Vector)
	}
	req := svc.lastRequest()
	if req.ModelName != "embedder" || req.Type != "openvino" || req.Digest != "sha256:test" || req.Path != "/models/embedder" {
		t.Fatalf("Embed request identity = %+v", req)
	}
	if req.Text != "query text" {
		t.Fatalf("Embed text = %q", req.Text)
	}
}

func TestSlotControlRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		slot.WithOwner("owner-1"),
		slot.WithBackend("llama"),
	)
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	ctx := context.Background()

	active, err := client.LoadModel(ctx, transport.LoadModelRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "a",
		Type:      "llama",
		Digest:    "digest-a",
		Path:      "/models/a.gguf",
		Config:    transport.Config{NumCtx: 100},
	})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if active.ModelName != "a" || active.Generation == 0 {
		t.Fatalf("active over wire = %+v", active)
	}

	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != transport.SlotReady || st.Active == nil || st.Active.Generation != active.Generation {
		t.Fatalf("status over wire = %+v", st)
	}

	sess, err := client.OpenSession(ctx, transport.OpenSessionRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "a",
		Type:      "llama",
		Digest:    "digest-a",
		Path:      "/models/a.gguf",
		Config:    transport.Config{NumCtx: 100},
	})
	if err != nil {
		t.Fatalf("OpenSession active: %v", err)
	}
	_, err = client.LoadModel(ctx, transport.LoadModelRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "b",
		Type:      "llama",
		Digest:    "digest-b",
		Path:      "/models/b.gguf",
		Config:    transport.Config{NumCtx: 100},
	})
	if !errors.Is(err, transport.ErrModelBusy) {
		t.Fatalf("LoadModel while session held = %v, want ErrModelBusy", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.UnloadModel(ctx, transport.UnloadModelRequest{ExpectedGeneration: active.Generation}); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
}

// LoRA adapter identity must survive the gRPC wire: a field silently dropped on
// the wire is an invisible cache-safety bug (a variant served as its base). This
// drives LoadModel with adapters through a real slot and asserts they come back on
// both the LoadModel reply and the Status snapshot.
func TestLoRAAdapterIdentityRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		slot.WithOwner("owner-1"),
		slot.WithBackend("llama"),
	)
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	ctx := context.Background()

	active, err := client.LoadModel(ctx, transport.LoadModelRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "a",
		Type:      "llama",
		Digest:    "digest-a",
		Path:      "/models/a.gguf",
		Config:    transport.Config{NumCtx: 100},
		Adapters:  []transport.AdapterSpec{{Name: "style", Path: "/adapters/style.gguf", Digest: "adapter-1", Scale: 1.5}},
	})
	if err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if len(active.Adapters) != 1 || active.Adapters[0].Digest != "adapter-1" || active.Adapters[0].Scale != 1.5 {
		t.Fatalf("adapters did not survive the wire into ActiveModel: %+v", active.Adapters)
	}

	st, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Active == nil || len(st.Active.Adapters) != 1 || st.Active.Adapters[0].Digest != "adapter-1" {
		t.Fatalf("adapters missing from Status.Active: %+v", st.Active)
	}
}

// imageRecordingSession records the prefix/suffix inputs it receives so a test
// can assert image parts survived the wire byte-exact.
type imageRecordingSession struct {
	decodeRecordingSession

	mu     sync.Mutex
	prefix transport.PrefixInput
	suffix transport.SuffixInput
}

func (s *imageRecordingSession) EnsurePrefix(_ context.Context, prefix transport.PrefixInput) (transport.PrefixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefix = prefix
	return transport.PrefixStatus{}, nil
}

func (s *imageRecordingSession) PrefillSuffix(_ context.Context, suffix transport.SuffixInput) (transport.SuffixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suffix = suffix
	return transport.SuffixStatus{}, nil
}

func (s *imageRecordingSession) lastInputs() (transport.PrefixInput, transport.SuffixInput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefix, s.suffix
}

// Image parts must survive the gRPC wire byte-exact: a corrupted or dropped
// image silently degrades a vision request into a text-only one. Binary-heavy
// payloads (including zero bytes) exercise the JSON codec's base64 path, and an
// image-less suffix must keep decoding to no images (backward compatibility).
func TestImagePartsRoundTripOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rec := &imageRecordingSession{}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess:          rec,
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0xFF, 0x00}
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}
	if _, err := sess.EnsurePrefix(context.Background(), transport.PrefixInput{
		Text:   "stable",
		Images: []transport.ImagePart{{Data: png, MimeType: "image/png"}},
	}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := sess.PrefillSuffix(context.Background(), transport.SuffixInput{
		Text: "look: " + transport.MediaMarker + " and " + transport.MediaMarker,
		Images: []transport.ImagePart{
			{Data: png, MimeType: "image/png"},
			{Data: jpg, MimeType: "image/jpeg"},
		},
	}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	prefix, suffix := rec.lastInputs()
	if len(prefix.Images) != 1 || !bytes.Equal(prefix.Images[0].Data, png) || prefix.Images[0].MimeType != "image/png" {
		t.Fatalf("prefix images over wire = %+v", prefix.Images)
	}
	if len(suffix.Images) != 2 ||
		!bytes.Equal(suffix.Images[0].Data, png) || suffix.Images[0].MimeType != "image/png" ||
		!bytes.Equal(suffix.Images[1].Data, jpg) || suffix.Images[1].MimeType != "image/jpeg" {
		t.Fatalf("suffix images over wire = %+v", suffix.Images)
	}

	if _, err := sess.PrefillSuffix(context.Background(), transport.SuffixInput{Text: "text only"}); err != nil {
		t.Fatalf("PrefillSuffix image-less: %v", err)
	}
	if _, suffix = rec.lastInputs(); len(suffix.Images) != 0 {
		t.Fatalf("image-less suffix decoded %d images, want none", len(suffix.Images))
	}
}

func TestServerClosesSlotSessionWhenClientConnectionEnds(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		slot.WithOwner("owner-1"),
		slot.WithBackend("llama"),
	)
	go func() { _ = transportgrpc.Serve(ctx, lis, svc, "owner-1", "llama") }()

	client1, err := transportgrpc.DialLeader(lis.Addr().String(), "owner-1")
	if err != nil {
		t.Fatalf("dial client1: %v", err)
	}
	if _, err := client1.OpenSession(ctx, transport.OpenSessionRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "a",
		Type:      "llama",
		Digest:    "digest-a",
		Path:      "/models/a.gguf",
		Config:    transport.Config{NumCtx: 100},
	}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := client1.Close(); err != nil {
		t.Fatalf("close client1 connection: %v", err)
	}

	client2, err := transportgrpc.DialLeader(lis.Addr().String(), "owner-1")
	if err != nil {
		t.Fatalf("dial client2: %v", err)
	}
	t.Cleanup(func() { _ = client2.Close() })

	loadReq := transport.LoadModelRequest{
		Fence:     transport.Fence{OwnerInstanceID: "owner-1"},
		ModelName: "b",
		Type:      "llama",
		Digest:    "digest-b",
		Path:      "/models/b.gguf",
		Config:    transport.Config{NumCtx: 100},
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		active, err := client2.LoadModel(ctx, loadReq)
		if err == nil {
			if active.ModelName != "b" {
				t.Fatalf("active model = %+v, want b", active)
			}
			return
		}
		lastErr = err
		if !errors.Is(err, transport.ErrModelBusy) {
			t.Fatalf("LoadModel after client disconnect = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("slot stayed busy after client disconnect: %v", lastErr)
}

func TestStreamChunkSentinelErrorOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &decodeRecordingService{
		MemoryService: transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		sess:          &streamErrorSession{err: transport.ErrSlotGenerationStale},
	}
	client := startServerWithService(t, svc, lis, "owner-1", "owner-1")
	sess, err := client.OpenSession(context.Background(), transport.OpenSessionRequest{
		Fence: transport.Fence{OwnerInstanceID: "owner-1"},
	})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	ch, err := sess.Decode(context.Background(), transport.DecodeConfig{})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	chunk, ok := <-ch
	if !ok {
		t.Fatal("Decode stream closed without chunk")
	}
	if !errors.Is(chunk.Error, transport.ErrSlotGenerationStale) {
		t.Fatalf("stream chunk error = %v, want ErrSlotGenerationStale", chunk.Error)
	}
}

// --- Node admin RPCs (ListModels/RemoveModel/DiskStats/PushModel) ---

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// startAdminServer wires a real slot.Service (which implements
// transport.NodeAdmin) over the wire, rooted at modelsDir. Unlike startServer
// (a bare MemoryService), the admin RPCs need slot.Service's NodeAdmin
// delegation to be reachable at all.
func startAdminServer(t *testing.T, modelsDir, owner string) *transportgrpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence(owner)),
		slot.WithOwner(owner),
		slot.WithBackend("llama"),
		slot.WithModelsDir(modelsDir),
	)
	return startServerWithService(t, svc, lis, owner, owner)
}

func writeAdminTestModel(t *testing.T, modelsDir, name string, content []byte) {
	t.Helper()
	dir := filepath.Join(modelsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.gguf"), content, 0o644); err != nil {
		t.Fatalf("write model file: %v", err)
	}
}

func TestListModelsRoundTripOverWire(t *testing.T) {
	dir := t.TempDir()
	writeAdminTestModel(t, dir, "a", []byte("weights-a"))
	client := startAdminServer(t, dir, "owner-1")

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "a" || models[0].Type != "llama" {
		t.Fatalf("ListModels = %+v, want one llama model named a", models)
	}
	if models[0].Digest == "" {
		t.Fatal("ListModels model missing digest")
	}
	if models[0].SizeBytes != int64(len("weights-a")) {
		t.Fatalf("ListModels size = %d, want %d", models[0].SizeBytes, len("weights-a"))
	}
}

func TestListModelsEmptyDirRoundTripOverWire(t *testing.T) {
	client := startAdminServer(t, t.TempDir(), "owner-1")
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ListModels = %+v, want empty", models)
	}
}

func TestRemoveModelRoundTripOverWire(t *testing.T) {
	dir := t.TempDir()
	writeAdminTestModel(t, dir, "a", []byte("weights-a"))
	client := startAdminServer(t, dir, "owner-1")

	if err := client.RemoveModel(context.Background(), "a"); err != nil {
		t.Fatalf("RemoveModel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("model dir still exists after RemoveModel: err=%v", err)
	}
}

func TestRemoveModelUnknownReturnsErrModelNotFoundOverWire(t *testing.T) {
	client := startAdminServer(t, t.TempDir(), "owner-1")
	err := client.RemoveModel(context.Background(), "missing")
	if !errors.Is(err, transport.ErrModelNotFound) {
		t.Fatalf("RemoveModel(missing) = %v, want ErrModelNotFound", err)
	}
}

func TestDiskStatsRoundTripOverWire(t *testing.T) {
	client := startAdminServer(t, t.TempDir(), "owner-1")
	st, err := client.DiskStats(context.Background())
	if err != nil {
		t.Fatalf("DiskStats: %v", err)
	}
	if st.TotalBytes <= 0 {
		t.Fatalf("DiskStats.TotalBytes = %d, want > 0", st.TotalBytes)
	}
}

func TestNodeAdminStaleFenceRejectedOverWire(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := slot.New(
		transport.NewMemoryService(transport.WithOwnerFence("owner-1")),
		slot.WithOwner("owner-1"),
		slot.WithBackend("llama"),
		slot.WithModelsDir(t.TempDir()),
	)
	// Dial with an owner the server does NOT hold the lease under: every
	// admin RPC must reject this the same way OpenSession does.
	client := startServerWithService(t, svc, lis, "owner-1", "wrong-owner")

	if _, err := client.ListModels(context.Background()); !errors.Is(err, transport.ErrStaleFence) {
		t.Fatalf("ListModels with wrong owner = %v, want ErrStaleFence", err)
	}
	if err := client.RemoveModel(context.Background(), "a"); !errors.Is(err, transport.ErrStaleFence) {
		t.Fatalf("RemoveModel with wrong owner = %v, want ErrStaleFence", err)
	}
	if _, err := client.DiskStats(context.Background()); !errors.Is(err, transport.ErrStaleFence) {
		t.Fatalf("DiskStats with wrong owner = %v, want ErrStaleFence", err)
	}
	_, err = client.PushModel(context.Background(), transport.PushManifest{
		Name: "a", Type: "llama", Format: transport.PushFormatFile,
	}, bytes.NewReader([]byte("x")))
	if !errors.Is(err, transport.ErrStaleFence) {
		t.Fatalf("PushModel with wrong owner = %v, want ErrStaleFence", err)
	}
}

func TestPushModelRoundTripOverWire_File(t *testing.T) {
	dir := t.TempDir()
	client := startAdminServer(t, dir, "owner-1")
	content := []byte("gguf-weights-content")
	digest := sha256Hex(content)

	res, err := client.PushModel(context.Background(), transport.PushManifest{
		Name:       "pushed",
		Type:       "llama",
		Digest:     digest,
		TotalBytes: int64(len(content)),
		Format:     transport.PushFormatFile,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("PushModel: %v", err)
	}
	if res.AlreadyPresent {
		t.Fatal("first push reported AlreadyPresent")
	}
	if res.BytesWritten != int64(len(content)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(content))
	}

	got, err := os.ReadFile(filepath.Join(dir, "pushed", "model.gguf"))
	if err != nil {
		t.Fatalf("read pushed model: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("pushed content = %q, want %q", got, content)
	}
}

func TestPushModelRoundTripOverWire_AlreadyPresentIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	client := startAdminServer(t, dir, "owner-1")
	content := []byte("gguf-weights-content")
	digest := sha256Hex(content)
	manifest := transport.PushManifest{Name: "pushed", Type: "llama", Digest: digest, Format: transport.PushFormatFile}

	if _, err := client.PushModel(context.Background(), manifest, bytes.NewReader(content)); err != nil {
		t.Fatalf("first PushModel: %v", err)
	}
	res, err := client.PushModel(context.Background(), manifest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second PushModel: %v", err)
	}
	if !res.AlreadyPresent {
		t.Fatal("repeat push with matching digest did not report AlreadyPresent")
	}
}

func TestPushModelRoundTripOverWire_DigestMismatchRejected(t *testing.T) {
	client := startAdminServer(t, t.TempDir(), "owner-1")
	content := []byte("gguf-weights-content")

	_, err := client.PushModel(context.Background(), transport.PushManifest{
		Name:   "pushed",
		Type:   "llama",
		Digest: "not-the-real-digest",
		Format: transport.PushFormatFile,
	}, bytes.NewReader(content))
	if !errors.Is(err, transport.ErrDigestMismatch) {
		t.Fatalf("PushModel with wrong digest = %v, want ErrDigestMismatch", err)
	}
}

func TestPushModelRoundTripOverWire_LargeContentSpansMultipleChunks(t *testing.T) {
	dir := t.TempDir()
	client := startAdminServer(t, dir, "owner-1")
	// Several times the wire chunk size, to force the client to split the
	// stream across many PushModel frames and the server to reassemble them.
	content := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16MiB
	digest := sha256Hex(content)

	res, err := client.PushModel(context.Background(), transport.PushManifest{
		Name:       "large",
		Type:       "llama",
		Digest:     digest,
		TotalBytes: int64(len(content)),
		Format:     transport.PushFormatFile,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("PushModel: %v", err)
	}
	if res.BytesWritten != int64(len(content)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(content))
	}
	got, err := os.ReadFile(filepath.Join(dir, "large", "model.gguf"))
	if err != nil {
		t.Fatalf("read pushed model: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("large pushed content mismatch")
	}
}

func TestPushModelRoundTripOverWire_UnsupportedTypeRejectedWithoutDeadlock(t *testing.T) {
	// Regression: the server must not hang when ReceiveModel rejects a
	// manifest before consuming the stream (see pushModel's io.Pipe doc
	// comment on defer pr.Close()).
	client := startAdminServer(t, t.TempDir(), "owner-1")
	content := bytes.Repeat([]byte("x"), 1<<20)

	done := make(chan error, 1)
	go func() {
		_, err := client.PushModel(context.Background(), transport.PushManifest{
			Name:   "bad",
			Type:   "ollama",
			Format: transport.PushFormatFile,
		}, bytes.NewReader(content))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PushModel with unsupported type = nil error, want rejection")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PushModel did not return within timeout — server deadlocked")
	}
}
