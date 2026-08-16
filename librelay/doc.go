// Package librelay defines the wire contract between a contenox runtime and a
// relay: the [Frame] envelope, its NDJSON codec ([Reader], [Writer]), and the
// relay-level control messages. A runtime dials out and holds the connection;
// nothing here listens. Control and tunnelled traffic share [Frame.Type], the
// protocol version is negotiated once in [Hello] / [Welcome], and unknown types
// and fields are never fatal.
package librelay
