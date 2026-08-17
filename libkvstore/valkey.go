package libkvstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

type VKManager struct {
	client    valkey.Client
	ttl       time.Duration
	namespace string
}

type Config struct {
	KVAddr      string `json:"kv_addr"`
	KVUsername  string `json:"kv_username,omitempty"`
	KVPassword  string `json:"kv_password,omitempty"`
	KVDB        int    `json:"kv_db,omitempty"`
	KVNamespace string `json:"kv_namespace,omitempty"`
}

func NewManager(cfg Config, ttl time.Duration) (*VKManager, error) {
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{cfg.KVAddr},
		Username:    cfg.KVUsername,
		Password:    cfg.KVPassword,
		SelectDB:    cfg.KVDB,
	})
	if err != nil {
		return nil, err
	}
	return &VKManager{
		client:    client,
		ttl:       ttl,
		namespace: keyNamespace(cfg.KVNamespace),
	}, nil
}

func keyNamespace(raw string) string {
	ns := strings.TrimSpace(raw)
	if ns == "" {
		return ""
	}
	return strings.TrimSuffix(ns, ":") + ":"
}

func (m *VKManager) Executor(ctx context.Context) (KVExecutor, error) {
	return &VKExecutor{
		client:    m.client,
		ttl:       m.ttl,
		namespace: m.namespace,
	}, nil
}

func (m *VKManager) Close() error {
	m.client.Close()
	return nil
}

type VKExecutor struct {
	client    valkey.Client
	ttl       time.Duration
	namespace string
}

func (r *VKExecutor) scoped(key Key) string { return r.namespace + string(key) }

func (r *VKExecutor) Get(ctx context.Context, key Key) (json.RawMessage, error) {
	cmd := r.client.B().Get().Key(r.scoped(key)).Build()
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
			Key(r.scoped(key)).
			Value(string(value)).
			PxMilliseconds(ttlMs).
			Build()
	} else {
		cmd = r.client.B().Set().
			Key(r.scoped(key)).
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
	cmd := r.client.B().Del().Key(r.scoped(key)).Build()
	_, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) Exists(ctx context.Context, key Key) (bool, error) {
	cmd := r.client.B().Exists().Key(r.scoped(key)).Build()
	res, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return false, errors.Join(ErrConnectionFailed, err)
	}
	return res > 0, nil
}

func (r *VKExecutor) Keys(ctx context.Context, pattern string) ([]Key, error) {
	cmd := r.client.B().Keys().Pattern(r.namespace + pattern).Build()
	strSlice, err := r.client.Do(ctx, cmd).AsStrSlice()
	if err != nil {
		return nil, errors.Join(ErrConnectionFailed, err)
	}

	keys := make([]Key, len(strSlice))
	for i, k := range strSlice {
		keys[i] = strings.TrimPrefix(k, r.namespace)
	}
	return keys, nil
}

func (r *VKExecutor) ListPush(ctx context.Context, key Key, value json.RawMessage) error {
	cmd := r.client.B().Lpush().
		Key(r.scoped(key)).
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
		Key(r.scoped(key)).
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
		Key(r.scoped(key)).
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
	cmd := r.client.B().Llen().Key(r.scoped(key)).Build()
	length, err := r.client.Do(ctx, cmd).AsInt64()
	if err != nil {
		return 0, errors.Join(ErrConnectionFailed, err)
	}
	return length, nil
}

func (r *VKExecutor) ListRPop(ctx context.Context, key Key) (json.RawMessage, error) {
	cmd := r.client.B().Rpop().Key(r.scoped(key)).Build()
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
		Key(r.scoped(key)).
		Member(string(member)).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func (r *VKExecutor) SetMembers(ctx context.Context, key Key) ([]json.RawMessage, error) {
	cmd := r.client.B().Smembers().Key(r.scoped(key)).Build()
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
		Key(r.scoped(key)).
		Member(string(member)).
		Build()
	err := r.client.Do(ctx, cmd).Error()
	if err != nil {
		return errors.Join(ErrConnectionFailed, err)
	}
	return nil
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
