package grpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sentinels maps the contract's canonical errors to a stable wire token + gRPC
// code so errors.Is keeps working across the boundary.
var sentinels = []struct {
	token string
	code  codes.Code
	err   error
}{
	{"context_canceled", codes.Canceled, context.Canceled},
	{"deadline_exceeded", codes.DeadlineExceeded, context.DeadlineExceeded},
	{"stale_fence", codes.FailedPrecondition, transport.ErrStaleFence},
	{"not_owner", codes.FailedPrecondition, transport.ErrNotOwner},
	{"session_closed", codes.FailedPrecondition, transport.ErrSessionClosed},
	{"context_overflow", codes.ResourceExhausted, transport.ErrContextOverflow},
	{"session_fatal", codes.Aborted, transport.ErrSessionFatal},
	{"model_busy", codes.FailedPrecondition, transport.ErrModelBusy},
	{"model_not_active", codes.FailedPrecondition, transport.ErrModelNotActive},
	{"model_switch_required", codes.FailedPrecondition, transport.ErrModelSwitchRequired},
	{"device_busy", codes.ResourceExhausted, transport.ErrDeviceBusy},
	{"model_load_failed", codes.Internal, transport.ErrModelLoadFailed},
	{"insufficient_memory", codes.ResourceExhausted, transport.ErrInsufficientMemory},
	{"slot_generation_stale", codes.FailedPrecondition, transport.ErrSlotGenerationStale},
	{"backend_mismatch", codes.FailedPrecondition, transport.ErrBackendMismatch},
	{"unsupported_feature", codes.Unimplemented, transport.ErrUnsupportedFeature},
	{"manifest_mismatch", codes.FailedPrecondition, contextasm.ErrManifestMismatch},
	{"model_not_found", codes.NotFound, transport.ErrModelNotFound},
	{"digest_mismatch", codes.FailedPrecondition, transport.ErrDigestMismatch},
	{"unsupported_model_type", codes.InvalidArgument, transport.ErrUnsupportedModelType},
}

// encodeError turns a contract error into a gRPC status carrying a sentinel
// token, so the client can reconstruct the original sentinel.
func encodeError(err error) error {
	if err == nil {
		return nil
	}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			st := status.New(s.code, s.token+": "+err.Error())
			if s.token == "context_overflow" {
				if detail, ok := transport.ContextOverflowDetailFromError(err); ok {
					if withDetail, detailErr := st.WithDetails(errorInfoForOverflow(s.token, detail)); detailErr == nil {
						return withDetail.Err()
					}
				}
			}
			return st.Err()
		}
	}
	return status.Error(codes.Internal, "internal: "+err.Error())
}

// decodeError reverses encodeError: a status whose message starts with a known
// token is rewrapped around the matching sentinel so errors.Is works client-side.
func decodeError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	token, rest := msg, msg
	if i := strings.Index(msg, ": "); i >= 0 {
		token, rest = msg[:i], msg[i+2:]
	}
	for _, s := range sentinels {
		if s.token == token {
			if s.token == "context_overflow" {
				if detail, ok := overflowDetailFromStatus(st); ok {
					return transport.NewContextOverflowError(detail.Stage, detail.ResidentTokens, detail.AdditionalTokens, detail.NumCtx)
				}
			}
			if s.token == "manifest_mismatch" {
				return contextasm.NewManifestMismatchError(rest)
			}
			return fmt.Errorf("%w: %s", s.err, rest)
		}
	}
	switch st.Code() {
	case codes.Canceled:
		return fmt.Errorf("%w: %s", context.Canceled, msg)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
	}
	return err
}

func errorToken(err error) string {
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return s.token
		}
	}
	return ""
}

func decodeWireError(token, msg string, detail *transport.ContextOverflowDetail) error {
	if msg == "" {
		return nil
	}
	for _, s := range sentinels {
		if s.token == token {
			if s.token == "context_overflow" && detail != nil {
				return transport.NewContextOverflowError(detail.Stage, detail.ResidentTokens, detail.AdditionalTokens, detail.NumCtx)
			}
			if s.token == "manifest_mismatch" {
				return contextasm.NewManifestMismatchError(msg)
			}
			return fmt.Errorf("%w: %s", s.err, msg)
		}
	}
	return errors.New(msg)
}

func errorInfoForOverflow(reason string, detail transport.ContextOverflowDetail) *errdetails.ErrorInfo {
	return &errdetails.ErrorInfo{
		Reason: reason,
		Domain: "contenox.transport",
		Metadata: map[string]string{
			"stage":             detail.Stage,
			"resident_tokens":   strconv.Itoa(detail.ResidentTokens),
			"additional_tokens": strconv.Itoa(detail.AdditionalTokens),
			"num_ctx":           strconv.Itoa(detail.NumCtx),
		},
	}
}

func overflowDetailFromStatus(st *status.Status) (transport.ContextOverflowDetail, bool) {
	for _, raw := range st.Details() {
		info, ok := raw.(*errdetails.ErrorInfo)
		if !ok || info.Reason != "context_overflow" {
			continue
		}
		detail, ok := overflowDetailFromMetadata(info.Metadata)
		if ok {
			return detail, true
		}
	}
	return transport.ContextOverflowDetail{}, false
}

func overflowDetailFromMetadata(md map[string]string) (transport.ContextOverflowDetail, bool) {
	if md == nil {
		return transport.ContextOverflowDetail{}, false
	}
	resident, residentOK := atoiMetadata(md["resident_tokens"])
	additional, additionalOK := atoiMetadata(md["additional_tokens"])
	numCtx, numCtxOK := atoiMetadata(md["num_ctx"])
	if !residentOK && !additionalOK && !numCtxOK && md["stage"] == "" {
		return transport.ContextOverflowDetail{}, false
	}
	return transport.ContextOverflowDetail{
		Stage:            md["stage"],
		ResidentTokens:   resident,
		AdditionalTokens: additional,
		NumCtx:           numCtx,
	}, true
}

func atoiMetadata(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
