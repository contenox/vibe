package libkvstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Predefined errors for key-value operations
var (
	ErrNotFound         = errors.New("key not found")
	ErrInvalidValue     = errors.New("invalid value for operation")
	ErrKeyExists        = errors.New("key already exists")
	ErrTxFailed         = errors.New("transaction failed")
	ErrTxConflict       = errors.New("transaction conflict")
	ErrLockUnavailable  = errors.New("lock unavailable")
	ErrConnectionFailed = errors.New("connection failed")
)

type Key = string

// KeyValue is used for backward compatibility with the old interface
type KeyValue struct {
	Key   string
	Value json.RawMessage
	TTL   time.Time
}

// KVManager defines the interface for obtaining executors
type KVManager interface {
	// Executor returns a non-transactional executor
	Executor(ctx context.Context) (KVExecutor, error)
	Close() error
}

// KVExecutor represents operations
type KVExecutor interface {
	// Basic operations
	Get(ctx context.Context, key Key) (json.RawMessage, error)
	Set(ctx context.Context, key Key, value json.RawMessage) error
	SetWithTTL(ctx context.Context, key Key, value json.RawMessage, ttl time.Duration) error
	Delete(ctx context.Context, key Key) error
	Exists(ctx context.Context, key Key) (bool, error)
	Keys(ctx context.Context, pattern string) ([]Key, error)

	// List operations
	ListPush(ctx context.Context, key Key, value json.RawMessage) error
	ListRange(ctx context.Context, key Key, start, stop int64) ([]json.RawMessage, error)
	ListTrim(ctx context.Context, key Key, start, stop int64) error
	ListLength(ctx context.Context, key Key) (int64, error)
	ListRPop(ctx context.Context, key Key) (json.RawMessage, error)

	// Set operations
	SetAdd(ctx context.Context, key Key, member json.RawMessage) error
	SetMembers(ctx context.Context, key Key) ([]json.RawMessage, error)
	SetRemove(ctx context.Context, key Key, member json.RawMessage) error
}
