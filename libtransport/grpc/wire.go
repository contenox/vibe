package grpc

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/contenox/contenox/libtransport"
	"github.com/contenox/contenox/libtracker"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// serviceName is the fully-qualified gRPC service for the session contract.
const serviceName = "contenox.transport.v1.Compute"

func method(name string) string { return "/" + serviceName + "/" + name }

// computeServer is the method set the handlers dispatch to. grpc.RegisterService
// requires ServiceDesc.HandlerType to be an interface that the registered impl
// (*Server) satisfies; this is that interface.
type computeServer interface {
	health(context.Context, *healthReq) (*healthResp, error)
	status(context.Context, *statusReq) (*transport.DaemonStatus, error)
	loadModel(context.Context, *loadModelReq) (*transport.ActiveModel, error)
	unloadModel(context.Context, *unloadModelReq) (*unloadModelResp, error)
	openSession(context.Context, *openSessionReq) (*openSessionResp, error)
	describe(context.Context, *openSessionReq) (*describeResp, error)
	embed(context.Context, *embedReq) (*embedResp, error)
	ensurePrefix(context.Context, *ensurePrefixReq) (*transport.PrefixStatus, error)
	prefillSuffix(context.Context, *prefillSuffixReq) (*transport.SuffixStatus, error)
	explainContext(context.Context, *explainReq) (*transport.ContextReport, error)
	snapshot(context.Context, *snapshotReq) (*transport.SessionSnapshot, error)
	restore(context.Context, *restoreReq) (*restoreResp, error)
	closeSession(context.Context, *closeReq) (*closeResp, error)
	decode(context.Context, *decodeReq, grpclib.ServerStream) error
	listModels(context.Context, *listModelsReq) (*listModelsResp, error)
	removeModel(context.Context, *removeModelReq) (*removeModelResp, error)
	diskStats(context.Context, *diskStatsReq) (*transport.NodeDiskStats, error)
	pushModel(context.Context, *pushModelChunk, grpclib.ServerStream) error
}

// Wire request/response payloads. Status/report responses reuse the transport.*
// types directly (they are JSON-friendly); only stream chunks need a wire form
// because StreamChunk carries an error value.

type openSessionReq struct {
	OwnerInstanceID string                  `json:"owner_instance_id,omitempty"`
	ModelName       string                  `json:"model_name,omitempty"`
	Type            string                  `json:"type,omitempty"`
	Digest          string                  `json:"digest,omitempty"`
	Path            string                  `json:"path,omitempty"`
	Config          transport.Config        `json:"config"`
	Adapters        []transport.AdapterSpec `json:"adapters,omitempty"`
}

type openSessionResp struct {
	Handle string `json:"handle"`
}

type statusReq struct {
	OwnerInstanceID string `json:"owner_instance_id,omitempty"`
}

type loadModelReq struct {
	OwnerInstanceID    string                  `json:"owner_instance_id,omitempty"`
	ModelName          string                  `json:"model_name,omitempty"`
	Type               string                  `json:"type,omitempty"`
	Digest             string                  `json:"digest,omitempty"`
	Path               string                  `json:"path,omitempty"`
	Config             transport.Config        `json:"config"`
	Adapters           []transport.AdapterSpec `json:"adapters,omitempty"`
	ExpectedGeneration uint64                  `json:"expected_generation,omitempty"`
}

type unloadModelReq struct {
	OwnerInstanceID    string `json:"owner_instance_id,omitempty"`
	ExpectedGeneration uint64 `json:"expected_generation,omitempty"`
}

type unloadModelResp struct{}

// describeReq reuses the open-session request shape: Type + Path identify the
// model, and Config carries the requested context/runtime knobs for capacity
// planning. describeResp carries the model capabilities the daemon read from
// model metadata plus the capacity decision.
type describeResp struct {
	ModelMaxContext                     int                    `json:"model_max_context"`
	EffectiveContext                    int                    `json:"effective_context"`
	MemoryContextTokens                 int                    `json:"memory_context_tokens,omitempty"`
	HotContextTokens                    int                    `json:"hot_context_tokens,omitempty"`
	PlannerEffectiveContext             int                    `json:"planner_effective_context,omitempty"`
	KVBytesPerToken                     int64                  `json:"kv_bytes_per_token,omitempty"`
	FreeBytes                           int64                  `json:"free_bytes,omitempty"`
	WeightsBytes                        int64                  `json:"weights_bytes,omitempty"`
	OverheadBytes                       int64                  `json:"overhead_bytes,omitempty"`
	ReservedBytes                       int64                  `json:"reserved_bytes,omitempty"`
	UserLimitBytes                      int64                  `json:"user_limit_bytes,omitempty"`
	MinFreeBytes                        int64                  `json:"min_free_bytes,omitempty"`
	HostColdBudgetBytes                 int64                  `json:"host_cold_budget_bytes,omitempty"`
	UsableBytes                         int64                  `json:"usable_bytes,omitempty"`
	RequiredBytes                       int64                  `json:"required_bytes,omitempty"`
	Clamped                             bool                   `json:"clamped,omitempty"`
	Reason                              string                 `json:"reason,omitempty"`
	DeviceKind                          string                 `json:"device_kind,omitempty"`
	DeviceID                            string                 `json:"device_id,omitempty"`
	DeviceTotalBytes                    int64                  `json:"device_total_bytes,omitempty"`
	SharedWithDisplay                   bool                   `json:"shared_with_display,omitempty"`
	RequestedGpuLayers                  int                    `json:"requested_gpu_layers,omitempty"`
	ResolvedGpuLayers                   int                    `json:"resolved_gpu_layers,omitempty"`
	SparseAttention                     bool                   `json:"sparse_attention,omitempty"`
	SlidingWindowAttentionTokens        int                    `json:"sliding_window_attention_tokens,omitempty"`
	SupportsVision                      bool                   `json:"supports_vision,omitempty"`
	VisionTokensPerImage                int                    `json:"vision_tokens_per_image,omitempty"`
	ChatTemplateFormat                  string                 `json:"chat_template_format,omitempty"`
	ChatTemplateThinkingStartTag        string                 `json:"chat_template_thinking_start_tag,omitempty"`
	ChatTemplateReasoningFormat         string                 `json:"chat_template_reasoning_format,omitempty"`
	ChatTemplateSupportsToolCalls       bool                   `json:"chat_template_supports_tool_calls,omitempty"`
	ChatTemplateSupportsThinking        bool                   `json:"chat_template_supports_thinking,omitempty"`
	ChatTemplateSupportsReasoningEffort bool                   `json:"chat_template_supports_reasoning_effort,omitempty"`
	RuntimeName                         string                 `json:"runtime_name,omitempty"`
	RuntimeDigest                       string                 `json:"runtime_digest,omitempty"`
	RuntimeSystemInfo                   string                 `json:"runtime_system_info,omitempty"`
	SupportsGPUOffload                  bool                   `json:"supports_gpu_offload,omitempty"`
	Devices                             []transport.DeviceInfo `json:"devices,omitempty"`
}

type embedReq struct {
	OwnerInstanceID string           `json:"owner_instance_id,omitempty"`
	ModelName       string           `json:"model_name,omitempty"`
	Type            string           `json:"type,omitempty"`
	Digest          string           `json:"digest,omitempty"`
	Path            string           `json:"path,omitempty"`
	Config          transport.Config `json:"config"`
	Text            string           `json:"text"`
}

type embedResp struct {
	Vector []float32 `json:"vector,omitempty"`
}

type ensurePrefixReq struct {
	Handle string                `json:"handle"`
	Prefix transport.PrefixInput `json:"prefix"`
}

type prefillSuffixReq struct {
	Handle string                `json:"handle"`
	Suffix transport.SuffixInput `json:"suffix"`
}

type decodeReq struct {
	Handle string                 `json:"handle"`
	Config transport.DecodeConfig `json:"config"`
}

type explainReq struct {
	Handle string `json:"handle"`
}

type snapshotReq struct {
	Handle string `json:"handle"`
}

type restoreReq struct {
	Handle   string                    `json:"handle"`
	Snapshot transport.SessionSnapshot `json:"snapshot"`
}

type restoreResp struct{}

type closeReq struct {
	Handle string `json:"handle"`
}

type closeResp struct{}

// Node admin RPCs (ListModels/RemoveModel/DiskStats/PushModel) manage the
// node's model store — see transport.NodeAdmin. They are fenced the same as
// every other RPC.

type listModelsReq struct {
	OwnerInstanceID string `json:"owner_instance_id,omitempty"`
}

type listModelsResp struct {
	Models []transport.NodeModel `json:"models,omitempty"`
}

type removeModelReq struct {
	OwnerInstanceID string `json:"owner_instance_id,omitempty"`
	Name            string `json:"name"`
}

type removeModelResp struct{}

type diskStatsReq struct {
	OwnerInstanceID string `json:"owner_instance_id,omitempty"`
}

// pushModelChunk is one frame of the PushModel client stream. Manifest is set
// only on the first frame the client sends; every frame (including the
// first, if it carries an initial slice of bytes) may carry Data. The server
// treats the byte stream as the concatenation of every frame's Data in
// arrival order.
type pushModelChunk struct {
	OwnerInstanceID string                  `json:"owner_instance_id,omitempty"`
	Manifest        *transport.PushManifest `json:"manifest,omitempty"`
	Data            []byte                  `json:"data,omitempty"`
}

type pushModelResp struct {
	AlreadyPresent bool  `json:"already_present,omitempty"`
	BytesWritten   int64 `json:"bytes_written,omitempty"`
}

// healthReq/healthResp back the unfenced liveness probe: it reports which owner
// instance is actually serving so a caller can confirm the lease holder is the
// process answering (and is ready), distinguishing a live owner from a wedged
// one that still holds a fresh lease.
type healthReq struct{}

type healthResp struct {
	InstanceID string `json:"instance_id,omitempty"`
	Ready      bool   `json:"ready"`
	Backend    string `json:"backend,omitempty"`
}

// wireChunk is the JSON-safe form of transport.StreamChunk (error -> string).
type wireChunk struct {
	Text        string                           `json:"text,omitempty"`
	Thinking    string                           `json:"thinking,omitempty"`
	ToolCalls   []transport.ToolCall             `json:"tool_calls,omitempty"`
	Error       string                           `json:"error,omitempty"`
	ErrorToken  string                           `json:"error_token,omitempty"`
	ErrorDetail *transport.ContextOverflowDetail `json:"error_detail,omitempty"`
}

// decodeStreamDesc is the client-side stream descriptor for Decode.
var decodeStreamDesc = grpclib.StreamDesc{StreamName: "Decode", ServerStreams: true}

// pushModelStreamDesc is the client-side stream descriptor for PushModel.
var pushModelStreamDesc = grpclib.StreamDesc{StreamName: "PushModel", ClientStreams: true}

// serviceDesc registers the unary methods plus the Decode server stream against
// a *Server.
var serviceDesc = grpclib.ServiceDesc{
	ServiceName: serviceName,
	HandlerType: (*computeServer)(nil),
	Methods: []grpclib.MethodDesc{
		{MethodName: "Health", Handler: unaryHandler("Health", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(healthReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.health(ctx, in)
		})},
		{MethodName: "Status", Handler: unaryHandler("Status", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(statusReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.status(ctx, in)
		})},
		{MethodName: "LoadModel", Handler: unaryHandler("LoadModel", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(loadModelReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.loadModel(ctx, in)
		})},
		{MethodName: "UnloadModel", Handler: unaryHandler("UnloadModel", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(unloadModelReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.unloadModel(ctx, in)
		})},
		{MethodName: "OpenSession", Handler: unaryHandler("OpenSession", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(openSessionReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.openSession(ctx, in)
		})},
		{MethodName: "Describe", Handler: unaryHandler("Describe", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(openSessionReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.describe(ctx, in)
		})},
		{MethodName: "Embed", Handler: unaryHandler("Embed", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(embedReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.embed(ctx, in)
		})},
		{MethodName: "EnsurePrefix", Handler: unaryHandler("EnsurePrefix", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(ensurePrefixReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.ensurePrefix(ctx, in)
		})},
		{MethodName: "PrefillSuffix", Handler: unaryHandler("PrefillSuffix", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(prefillSuffixReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.prefillSuffix(ctx, in)
		})},
		{MethodName: "ExplainContext", Handler: unaryHandler("ExplainContext", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(explainReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.explainContext(ctx, in)
		})},
		{MethodName: "Snapshot", Handler: unaryHandler("Snapshot", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(snapshotReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.snapshot(ctx, in)
		})},
		{MethodName: "Restore", Handler: unaryHandler("Restore", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(restoreReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.restore(ctx, in)
		})},
		{MethodName: "CloseSession", Handler: unaryHandler("CloseSession", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(closeReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.closeSession(ctx, in)
		})},
		{MethodName: "ListModels", Handler: unaryHandler("ListModels", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(listModelsReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.listModels(ctx, in)
		})},
		{MethodName: "RemoveModel", Handler: unaryHandler("RemoveModel", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(removeModelReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.removeModel(ctx, in)
		})},
		{MethodName: "DiskStats", Handler: unaryHandler("DiskStats", func(s *Server, ctx context.Context, dec func(any) error) (any, error) {
			in := new(diskStatsReq)
			if err := dec(in); err != nil {
				return nil, err
			}
			return s.diskStats(ctx, in)
		})},
	},
	Streams: []grpclib.StreamDesc{
		{
			StreamName:    "Decode",
			ServerStreams: true,
			Handler: func(srv any, stream grpclib.ServerStream) (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = recoveredHandlerPanic(stream.Context(), trackerOf(srv), "Decode", r)
					}
				}()
				in := new(decodeReq)
				if err := stream.RecvMsg(in); err != nil {
					return err
				}
				return srv.(*Server).decode(stream.Context(), in, stream)
			},
		},
		{
			StreamName:    "PushModel",
			ClientStreams: true,
			Handler: func(srv any, stream grpclib.ServerStream) (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = recoveredHandlerPanic(stream.Context(), trackerOf(srv), "PushModel", r)
					}
				}()
				in := new(pushModelChunk)
				if err := stream.RecvMsg(in); err != nil {
					return err
				}
				return srv.(*Server).pushModel(stream.Context(), in, stream)
			},
		},
	},
	Metadata: "contenox/transport",
}

// unaryHandler adapts a typed (*Server, ctx, dec) func to grpc's methodHandler
// signature. Fencing and error mapping live inside the server methods, so no
// unary interceptor is configured and the interceptor argument is unused. A
// recover guards every handler so a single malformed request cannot unwind a
// panic through grpc's per-stream goroutine and crash the daemon.
func unaryHandler(name string, call func(*Server, context.Context, func(any) error) (any, error)) func(any, context.Context, func(any) error, grpclib.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, _ grpclib.UnaryServerInterceptor) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoveredHandlerPanic(ctx, trackerOf(srv), name, r)
			}
		}()
		return call(srv.(*Server), ctx, dec)
	}
}

// trackerOf recovers srv's ActivityTracker for a recovered-panic report. srv is
// always a *Server in practice (grpc dispatches to the registered handler
// type); the fallback exists only so a panic recovery path can never itself
// panic on a bad type assertion.
func trackerOf(srv any) libtracker.ActivityTracker {
	if s, ok := srv.(*Server); ok && s.tracker != nil {
		return s.tracker
	}
	return libtracker.NoopTracker{}
}

// recoveredHandlerPanic converts a recovered handler panic into an Internal gRPC
// error instead of letting it crash the whole daemon (which would drop the
// resident model and the lease for every client). The panic and its stack are
// reported through the tracker so the fault stays diagnosable.
func recoveredHandlerPanic(ctx context.Context, tracker libtracker.ActivityTracker, method string, r any) error {
	reportErr, _, end := tracker.Start(ctx, "modeld_transport", "handler_panic", "method", method, "stack", string(debug.Stack()))
	reportErr(fmt.Errorf("panic: %v", r))
	end()
	return status.Errorf(codes.Internal, "internal: handler %s panicked: %v", method, r)
}
