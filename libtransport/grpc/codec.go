// Package grpc is the gRPC wire transport for the runtime/transport.Service
// contract. It lives in the runtime (which owns the contract) and is imported by
// modeld to serve it, so the dependency arrow stays one-way: modeld -> runtime.
// Both the server (serves any transport.Service) and the client (dials the lease
// leader and implements transport.Service/Session) live here.
//
// The bindings are hand-written against a registered JSON codec rather than
// generated from a .proto, so the transport is live without a protoc toolchain.
// Payloads are plain Go structs (the transport.* types are JSON-friendly).
package grpc

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// codecName is the gRPC content subtype the hand-written bindings use. Calls set
// it via grpc.CallContentSubtype so both ends select this JSON codec.
const codecName = "contenoxtransportjson"

func init() { encoding.RegisterCodec(jsonCodec{}) }

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return codecName }
