package planservice

import (
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/taskengine"
)

func Test_executorCompileCacheKey_changesWithTokenLimit(t *testing.T) {
	t.Parallel()
	a := &taskengine.TaskChainDefinition{ID: "chain-step-executor", TokenLimit: 131072}
	b := &taskengine.TaskChainDefinition{ID: "chain-step-executor", TokenLimit: 400000}
	ka, err := executorCompileCacheKey(a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := executorCompileCacheKey(b)
	if err != nil {
		t.Fatal(err)
	}
	if ka == kb {
		t.Fatalf("cache key should differ when token_limit differs: %q vs %q", ka, kb)
	}
}

func Test_executorCompileCacheKey_stableJSON(t *testing.T) {
	t.Parallel()
	ex := &taskengine.TaskChainDefinition{ID: "x", TokenLimit: 100}
	k1, _ := executorCompileCacheKey(ex)
	raw, _ := json.Marshal(ex)
	var ex2 taskengine.TaskChainDefinition
	_ = json.Unmarshal(raw, &ex2)
	k2, _ := executorCompileCacheKey(&ex2)
	if k1 != k2 {
		t.Fatalf("same logical chain should produce same key: %q vs %q", k1, k2)
	}
}
