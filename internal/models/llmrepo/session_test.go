package llmrepo

import (
	"context"
	"encoding/json"
	"testing"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

func TestUnit_DeriveSessionKey(t *testing.T) {
	if got := DeriveSessionKey(""); got != "" {
		t.Fatalf("empty session ID must derive to empty key (stateless), got %q", got)
	}
	a1 := DeriveSessionKey("session-a")
	a2 := DeriveSessionKey("session-a")
	b := DeriveSessionKey("session-b")
	if a1 == "" || a1 != a2 {
		t.Fatalf("key derivation must be stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("distinct sessions must derive distinct keys")
	}
	if a1 == "session-a" {
		t.Fatalf("the raw session ID must never be the key (it can reach providers)")
	}
}

func TestUnit_SessionKeyContext_RoundTripAndPrecedence(t *testing.T) {
	ctx := context.Background()
	if got := SessionKeyFromContext(ctx); got != "" {
		t.Fatalf("no key set, want empty, got %q", got)
	}
	if withEmpty := WithSessionKey(ctx, ""); withEmpty != ctx {
		t.Fatalf("empty key must not allocate a context value")
	}
	ctx = WithSessionKey(ctx, "ctx-key")
	if got := SessionKeyFromContext(ctx); got != "ctx-key" {
		t.Fatalf("round trip failed, got %q", got)
	}

	if got := effectiveSessionKey(ctx, Request{SessionKey: "req-key"}); got != "req-key" {
		t.Fatalf("explicit request key must win, got %q", got)
	}
	if got := effectiveSessionKey(ctx, Request{}); got != "ctx-key" {
		t.Fatalf("context key must apply when the request has none, got %q", got)
	}
}

func fnTool(typ, name string) libmodelprovider.Tool {
	return libmodelprovider.Tool{Type: typ, Function: &libmodelprovider.FunctionTool{Name: name}}
}

func applyAll(opts []libmodelprovider.ChatArgument) libmodelprovider.ChatConfig {
	cfg := libmodelprovider.ChatConfig{}
	for _, o := range opts {
		o.Apply(&cfg)
	}
	return cfg
}

// Same registry contents advertised in two different enumeration orders must serialize byte-identically after canonicalization.
func TestUnit_CanonicalToolOrder_ByteIdenticalAcrossInputOrder(t *testing.T) {
	hints := providerCacheHints(nil, "")
	orderA := withCanonicalRequestShape([]libmodelprovider.ChatArgument{
		libmodelprovider.WithTools(fnTool("function", "local_fs.read"), fnTool("function", "local_shell.exec")),
		libmodelprovider.WithTool(fnTool("function", "local_fs.write")),
	}, hints)
	orderB := withCanonicalRequestShape([]libmodelprovider.ChatArgument{
		libmodelprovider.WithTools(fnTool("function", "local_shell.exec"), fnTool("function", "local_fs.write")),
		libmodelprovider.WithTool(fnTool("function", "local_fs.read")),
	}, hints)

	jsonA, err := json.Marshal(applyAll(orderA).Tools)
	if err != nil {
		t.Fatal(err)
	}
	jsonB, err := json.Marshal(applyAll(orderB).Tools)
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonA) != string(jsonB) {
		t.Fatalf("tool list bytes depend on assembly order:\nA: %s\nB: %s", jsonA, jsonB)
	}

	cfg := applyAll(orderA)
	wantFirst := "local_fs.read"
	if cfg.Tools[0].Function.Name != wantFirst {
		t.Fatalf("expected sorted order starting with %s, got %s", wantFirst, cfg.Tools[0].Function.Name)
	}
}

func TestUnit_CanonicalToolOrder_HandlesNilFunction(t *testing.T) {
	tools := []libmodelprovider.Tool{
		fnTool("function", "b"),
		{Type: "function"}, // no Function block — must not panic
		fnTool("function", "a"),
	}
	sortToolsCanonically(tools)
	if tools[0].Function != nil {
		t.Fatalf("nil-function tool sorts first (empty name), got %+v", tools[0])
	}
}

func TestUnit_WithCanonicalRequestShape_DoesNotMutateCaller(t *testing.T) {
	orig := make([]libmodelprovider.ChatArgument, 1, 4)
	orig[0] = libmodelprovider.WithTemperature(0.5)
	out := withCanonicalRequestShape(orig, providerCacheHints(nil, ""))
	if len(out) != 3 {
		t.Fatalf("expected two appended arguments (hints + tool order), got %d", len(out))
	}
	if len(orig) != 1 {
		t.Fatalf("caller slice length changed")
	}
	if cap(orig) >= 2 && &orig[:2][1] == &out[1] {
		t.Fatalf("appended into the caller's backing array")
	}
}

// With no caller hints, llmrepo synthesizes StableSystem+StableTools with no history assertion, keyed by the resolved session key.
func TestUnit_ProviderCacheHints_DefaultsAreConservativeProducer(t *testing.T) {
	h := providerCacheHints(nil, "session-key-hash")
	if !h.StableSystem || !h.StableTools {
		t.Fatalf("defaults must assert system+tools stability, got %+v", h)
	}
	if h.StableHistoryLen != 0 {
		t.Fatalf("defaults must not assert a stable history prefix, got %d", h.StableHistoryLen)
	}
	if h.SessionKey != "session-key-hash" {
		t.Fatalf("session key must flow into the provider hints, got %q", h.SessionKey)
	}
	if h.TTL != 0 {
		t.Fatalf("defaults must leave TTL at the provider default, got %v", h.TTL)
	}
}

func TestUnit_ProviderCacheHints_ExplicitHintsTranslate(t *testing.T) {
	h := providerCacheHints(&CacheHints{
		StableSystem:     false,
		StableTools:      true,
		StableHistoryLen: 7,
		TTL:              "1h",
	}, "k")
	if h.StableSystem {
		t.Fatalf("explicit hints must override the defaults, got %+v", h)
	}
	if !h.StableTools || h.StableHistoryLen != 7 || h.SessionKey != "k" {
		t.Fatalf("translation lost fields: %+v", h)
	}
	if h.TTL.Hours() != 1 {
		t.Fatalf("TTL must parse, got %v", h.TTL)
	}
	if got := providerCacheHints(&CacheHints{TTL: "soon"}, ""); got.TTL != 0 {
		t.Fatalf("invalid TTL must degrade to zero, got %v", got.TTL)
	}
}

func TestUnit_WithCanonicalRequestShape_AppliesHintsToConfig(t *testing.T) {
	opts := withCanonicalRequestShape(nil, providerCacheHints(nil, "affinity"))
	cfg := applyAll(opts)
	if cfg.CacheHints == nil {
		t.Fatal("cache hints must land on the chat config")
	}
	if cfg.CacheHints.SessionKey != "affinity" || !cfg.CacheHints.StableSystem || !cfg.CacheHints.StableTools {
		t.Fatalf("hints malformed on config: %+v", cfg.CacheHints)
	}
}

func TestUnit_MergeTokenUsage_CacheFields(t *testing.T) {
	var dst libmodelprovider.TokenUsage
	mergeTokenUsage(&dst, &libmodelprovider.TokenUsage{PromptTokens: 100, CacheReadTokens: 80})
	mergeTokenUsage(&dst, &libmodelprovider.TokenUsage{CompletionTokens: 5, CacheWriteTokens: 20})
	if dst.CacheReadTokens != 80 || dst.CacheWriteTokens != 20 {
		t.Fatalf("cache fields must accumulate across split reports, got %+v", dst)
	}
}

func TestUnit_MergeTokenUsage_ZeroMeansNotReported(t *testing.T) {
	var dst libmodelprovider.TokenUsage
	mergeTokenUsage(&dst, &libmodelprovider.TokenUsage{PromptTokens: 100})
	mergeTokenUsage(&dst, &libmodelprovider.TokenUsage{CompletionTokens: 7, TotalTokens: 107})
	if dst.PromptTokens != 100 || dst.CompletionTokens != 7 || dst.TotalTokens != 107 {
		t.Fatalf("split reports must accumulate, got %+v", dst)
	}
	mergeTokenUsage(&dst, &libmodelprovider.TokenUsage{CompletionTokens: 9})
	if dst.PromptTokens != 100 || dst.CompletionTokens != 9 {
		t.Fatalf("later nonzero fields override, zero fields keep prior values, got %+v", dst)
	}
}
