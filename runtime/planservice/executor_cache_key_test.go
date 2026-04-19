package planservice

import (
	"encoding/json"
	"testing"

	"github.com/contenox/contenox/runtime/taskengine"
)

func Test_compileCacheKey_changesWithExecutorTokenLimit(t *testing.T) {
	t.Parallel()
	sum := &taskengine.TaskChainDefinition{ID: "chain-step-summarizer", TokenLimit: 131072}
	a := &taskengine.TaskChainDefinition{ID: "chain-step-executor", TokenLimit: 131072}
	b := &taskengine.TaskChainDefinition{ID: "chain-step-executor", TokenLimit: 400000}
	ka, err := compileCacheKey(a, sum)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := compileCacheKey(b, sum)
	if err != nil {
		t.Fatal(err)
	}
	if ka == kb {
		t.Fatalf("cache key should differ when executor token_limit differs: %q vs %q", ka, kb)
	}
}

func Test_compileCacheKey_changesWithSummarizerTokenLimit(t *testing.T) {
	t.Parallel()
	ex := &taskengine.TaskChainDefinition{ID: "chain-step-executor", TokenLimit: 131072}
	a := &taskengine.TaskChainDefinition{ID: "chain-step-summarizer", TokenLimit: 131072}
	b := &taskengine.TaskChainDefinition{ID: "chain-step-summarizer", TokenLimit: 200000}
	ka, err := compileCacheKey(ex, a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := compileCacheKey(ex, b)
	if err != nil {
		t.Fatal(err)
	}
	if ka == kb {
		t.Fatalf("cache key should differ when summarizer token_limit differs: %q vs %q", ka, kb)
	}
}

func Test_compileCacheKey_stableJSON(t *testing.T) {
	t.Parallel()
	ex := &taskengine.TaskChainDefinition{ID: "x", TokenLimit: 100}
	sum := &taskengine.TaskChainDefinition{ID: "s", TokenLimit: 100}
	k1, _ := compileCacheKey(ex, sum)

	raw, _ := json.Marshal(ex)
	var ex2 taskengine.TaskChainDefinition
	_ = json.Unmarshal(raw, &ex2)

	sumRaw, _ := json.Marshal(sum)
	var sum2 taskengine.TaskChainDefinition
	_ = json.Unmarshal(sumRaw, &sum2)

	k2, _ := compileCacheKey(&ex2, &sum2)
	if k1 != k2 {
		t.Fatalf("same logical chains should produce same key: %q vs %q", k1, k2)
	}
}
