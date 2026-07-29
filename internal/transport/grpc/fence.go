package grpc

import (
	"context"

	"github.com/contenox/contenox/internal/transport"
	"google.golang.org/grpc/metadata"
)

// ownerMetadataKey carries the owner instance id the client expects to be
// serving it. The client attaches it to every call; the server rejects a
// mismatch so a client never acts on a stale owner after a lease takeover.
const ownerMetadataKey = "x-contenox-owner-instance"

// withOwner attaches the expected owner instance id to an outgoing call.
func withOwner(ctx context.Context, owner string) context.Context {
	if owner == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, ownerMetadataKey, owner)
}

// checkFence verifies the incoming call carries this owner's instance id. An
// empty instanceID disables fencing (the unwired/local placeholder path).
func checkFence(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return transport.ErrStaleFence
	}
	vals := md.Get(ownerMetadataKey)
	if len(vals) != 1 || vals[0] != instanceID {
		return transport.ErrStaleFence
	}
	return nil
}
