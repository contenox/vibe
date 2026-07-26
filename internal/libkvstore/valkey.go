package libkvstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/valkey-io/valkey-go"
)

type VKManager struct {
	client valkey.Client
	ttl    time.Duration
}

type Config struct {
	KVAddr     string `json:"kv_addr"`
	KVPassword string `json:"kv_password,omitempty"`
}

func NewManager(cfg Config, ttl time.Duration) (*VKManager, error) {
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{cfg.KVAddr},
		Password:    cfg.KVPassword,
	})
	if err != nil {
		return nil, err
	}
	return &VKManager{
		client: client,
		ttl:    ttl,
	}, nil
}

func (m *VKManager) Executor(ctx context.Context) (KVExecutor, error) {
	return &VKExecutor{
		client: m.client,
		ttl:    m.ttl,
	}, nil
}

func (m *VKManager) Close() error {
	m.client.Close()
	return nil
}

// VKExecutor implements KVExecutor for Valkey
type VKExecutor struct {
	client valkey.Client
	ttl    time.Duration
}

func (r *VKExecutor) Get(ctx context.Context, key Key) (json.RawMessage, error) {
	cmd := r.client.B().Get().Key(string(key)).Build()
	res, err := r.client.Do(ctx, cmd).AsBytes()

	switch {
	case valkey.IsValkeyNil(err):
		return nil, ErrNotFound
	case errors.Is(err, context.Canceled):
		return nil, context.Canceled
	case err != nil:
		return nil, errors.Join(ErrConnectionFailed, err)
	default:
		return res, nil
	}
}

func (r *VKExecutor) Set(ctx context.Context, key Key, value json.RawMessage) error {
	return r.SetWithTTL(ctx, key, value, 0)
}

func (r *VKExecutor) SetWithTTL(ctx context.Context, key Key, value json.RawMessage, ttl time.Duration) error {
	if ttl <= 0 && r.ttl > 0 {
		ttl = r.ttl
	}

	var cmd valkey.Completed
	if ttl > 0 {
		ttlMs := max(ttl.Milliseconds(), 1)
		cmd = r.client.B().Set().
			Key(string(key)).
			Value(string(value)).
			PxMilliseconds(ttlMs).
			Build()
	} else {
		cmd = r.client.B().Set().
			Key(string(key)).
			Value(string(value)).
			Build()
	}

	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) Delete(ctx context.Context, key Key) error {
	cmd := r.client.B().Del().Key(string(key)).Build()
	_, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) Exists(ctx context.Context, key Key) (bool, error) {
	cmd := r.client.B().Exists().Key(string(key)).Build()
	res, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return false, errors.Join(ErrConnectionFailed, err)
	}
	return res > 0, nil
}

func (r *VKExecutor) Keys(ctx context.Context, pattern string) ([]Key, error) {
	cmd := r.client.B().Keys().Pattern(pattern).Build()
	strSlice, err := r.client.Do(ctx, cmd).AsStrSlice()
	if err != nil {
		return nil, errors.Join(ErrConnectionFailed, err)
	}

	keys := make([]Key, len(strSlice))
	copy(keys, strSlice)
	return keys, nil
}

func (r *VKExecutor) ListPush(ctx context.Context, key Key, value json.RawMessage) error {
	cmd := r.client.B().Lpush().
		Key(string(key)).
		Element(string(value)).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) ListRange(ctx context.Context, key Key, start, stop int64) ([]json.RawMessage, error) {
	cmd := r.client.B().Lrange().
		Key(string(key)).
		Start(start).
		Stop(stop).
		Build()

	strSlice, err := r.client.Do(ctx, cmd).AsStrSlice()
	if err != nil {
		return nil, errors.Join(ErrConnectionFailed, err)
	}

	result := make([]json.RawMessage, len(strSlice))
	for i, s := range strSlice {
		result[i] = []byte(s)
	}
	return result, nil
}

func (r *VKExecutor) ListTrim(ctx context.Context, key Key, start, stop int64) error {
	cmd := r.client.B().Ltrim().
		Key(string(key)).
		Start(start).
		Stop(stop).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) ListLength(ctx context.Context, key Key) (int64, error) {
	cmd := r.client.B().Llen().Key(string(key)).Build()
	length, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return 0, errors.Join(ErrConnectionFailed, err)
	}
	return length, nil
}

func (r *VKExecutor) ListRPop(ctx context.Context, key Key) (json.RawMessage, error) {
	cmd := r.client.B().Rpop().Key(string(key)).Build()
	res, err := r.client.Do(ctx, cmd).AsBytes()
	switch {
	case valkey.IsValkeyNil(err):
		return nil, ErrNotFound
	case errors.Is(err, context.Canceled):
		return nil, context.Canceled
	case err != nil:
		return nil, errors.Join(ErrConnectionFailed, err)
	default:
		return res, nil
	}
}

func (r *VKExecutor) SetAdd(ctx context.Context, key Key, member json.RawMessage) error {
	cmd := r.client.B().Sadd().
		Key(string(key)).
		Member(string(member)).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) SetMembers(ctx context.Context, key Key) ([]json.RawMessage, error) {
	cmd := r.client.B().Smembers().Key(string(key)).Build()
	strSlice, err := r.client.Do(ctx, cmd).AsStrSlice()
	if err != nil {
		return nil, errors.Join(ErrConnectionFailed, err)
	}

	result := make([]json.RawMessage, len(strSlice))
	for i, s := range strSlice {
		result[i] = []byte(s)
	}
	return result, nil
}

func (r *VKExecutor) SetRemove(ctx context.Context, key Key, member json.RawMessage) error {
	cmd := r.client.B().Srem().
		Key(string(key)).
		Member(string(member)).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

// Utility function
func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
