package libkvstore_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	libkv "github.com/contenox/contenox/internal/libkvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/valkey"
)

func SetupLocalValKeyInstance(ctx context.Context) (string, testcontainers.Container, func(), error) {
	cleanup := func() {}

	container, err := valkey.Run(ctx, "docker.io/valkey/valkey:7.2.5")
	if err != nil {
		return "", nil, cleanup, err
	}

	cleanup = func() {
		timeout := time.Second
		err := container.Stop(ctx, &timeout)
		if err != nil {
			panic(err)
		}
	}

	conn, err := container.ConnectionString(ctx)
	if err != nil {
		return "", nil, cleanup, err
	}
	return conn, container, cleanup, nil
}

func TestUnit_ValkeyCRUD(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	connStr, _, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	addr := u.Host

	cfg := libkv.Config{
		KVAddr:     addr,
		KVPassword: "",
	}
	manager, err := libkv.NewManager(cfg, 10*time.Second)
	require.NoError(t, err)
	defer manager.Close()

	kv, err := manager.Executor(ctx)
	require.NoError(t, err)

	key := "testkey"
	value := json.RawMessage(`"testvalue"`)

	err = kv.Set(ctx, key, value)
	require.NoError(t, err)

	retrieved, err := kv.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, value, retrieved)

	exists, err := kv.Exists(ctx, key)
	require.NoError(t, err)
	assert.True(t, exists)

	err = kv.Delete(ctx, key)
	require.NoError(t, err)

	_, err = kv.Get(ctx, key)
	assert.ErrorIs(t, err, libkv.ErrNotFound)

	exists, err = kv.Exists(ctx, key)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestUnit_ValkeyTTL(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	connStr, _, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	addr := u.Host

	cfg := libkv.Config{
		KVAddr:     addr,
		KVPassword: "",
	}
	manager, err := libkv.NewManager(cfg, 10*time.Second)
	require.NoError(t, err)
	defer manager.Close()

	kv, err := manager.Executor(ctx)
	require.NoError(t, err)

	key := "ttlkey"
	value := json.RawMessage(`"ttlvalue"`)

	err = kv.SetWithTTL(ctx, key, value, 2*time.Second)
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	_, err = kv.Get(ctx, key)
	assert.ErrorIs(t, err, libkv.ErrNotFound)
}

func TestUnit_ValkeyList(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	connStr, _, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	addr := u.Host

	cfg := libkv.Config{
		KVAddr:     addr,
		KVPassword: "",
	}
	manager, err := libkv.NewManager(cfg, 10*time.Second)
	require.NoError(t, err)
	defer manager.Close()

	kv, err := manager.Executor(ctx)
	require.NoError(t, err)

	keys := []string{"key1", "key2", "key3"}
	value := json.RawMessage(`"value"`)

	for _, key := range keys {
		err := kv.Set(ctx, key, value)
		require.NoError(t, err)
	}

	listed, err := kv.Keys(ctx, "*")
	require.NoError(t, err)

	listedMap := make(map[string]bool)
	for _, k := range listed {
		listedMap[k] = true
	}

	for _, key := range keys {
		assert.True(t, listedMap[key])
	}
}

func TestUnit_ValkeyListOperations(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	connStr, _, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	addr := u.Host

	cfg := libkv.Config{
		KVAddr:     addr,
		KVPassword: "",
	}
	manager, err := libkv.NewManager(cfg, 10*time.Second)
	require.NoError(t, err)
	defer manager.Close()

	kv, err := manager.Executor(ctx)
	require.NoError(t, err)

	listKey := "testlist"

	values := []json.RawMessage{
		json.RawMessage(`"item1"`),
		json.RawMessage(`"item2"`),
		json.RawMessage(`"item3"`),
	}

	for _, v := range values {
		err := kv.ListPush(ctx, listKey, v)
		require.NoError(t, err)
	}

	items, err := kv.ListRange(ctx, listKey, 0, -1)
	require.NoError(t, err)
	assert.Equal(t, len(values), len(items))

	for i, expected := range []string{"item3", "item2", "item1"} {
		var actual string
		err := json.Unmarshal(items[i], &actual)
		require.NoError(t, err)
		assert.Equal(t, expected, actual)
	}

	popped, err := kv.ListRPop(ctx, listKey)
	require.NoError(t, err)

	var poppedValue string
	err = json.Unmarshal(popped, &poppedValue)
	require.NoError(t, err)
	assert.Equal(t, "item1", poppedValue)

	length, err := kv.ListLength(ctx, listKey)
	require.NoError(t, err)
	assert.Equal(t, int64(2), length)
}

func TestUnit_ValkeySetOperations(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	connStr, _, cleanup, err := SetupLocalValKeyInstance(ctx)
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	addr := u.Host

	cfg := libkv.Config{
		KVAddr:     addr,
		KVPassword: "",
	}
	manager, err := libkv.NewManager(cfg, 10*time.Second)
	require.NoError(t, err)
	defer manager.Close()

	kv, err := manager.Executor(ctx)
	require.NoError(t, err)

	setKey := "testset"

	members := []json.RawMessage{
		json.RawMessage(`"member1"`),
		json.RawMessage(`"member2"`),
		json.RawMessage(`"member3"`),
	}

	for _, m := range members {
		err := kv.SetAdd(ctx, setKey, m)
		require.NoError(t, err)
	}

	setMembers, err := kv.SetMembers(ctx, setKey)
	require.NoError(t, err)
	assert.Equal(t, len(members), len(setMembers))

	memberMap := make(map[string]bool)
	for _, m := range setMembers {
		var s string
		err := json.Unmarshal(m, &s)
		require.NoError(t, err)
		memberMap[s] = true
	}

	for _, expected := range []string{"member1", "member2", "member3"} {
		assert.True(t, memberMap[expected])
	}
}
