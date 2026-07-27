package llmrepo

// This file is the engine-side groundwork for provider KV-cache utilization
// (docs/development/blueprints/provider-kv-cache.md). It owns three concerns:
//
//  1. Session identity for cache affinity: an opaque SessionKey derived from
//     the internal session ID, threaded to the resolver so a session sticks
//     to one provider/backend (blueprint §4.1.5, fixes P1).
//  2. Canonical tool ordering: the last-applied ChatArgument that guarantees
//     the tool list every adapter serializes is byte-stable across turns
//     (blueprint §4.1.2, fixes E7).
//  3. CacheHints: the thin, omission-tolerant stable/volatile-boundary
//     declaration that adapters translate into provider cache controls in a
//     later slice (blueprint §4.2).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	libmodelprovider "github.com/contenox/beam/internal/models/modelrepo"
)

// sessionKeyContextKeyType is unexported so no other package can collide with
// the key; the only way in or out is WithSessionKey / SessionKeyFromContext.
type sessionKeyContextKeyType struct{}

var sessionKeyContextKey sessionKeyContextKeyType

// WithSessionKey stores an already-derived session cache key on the context.
//
// Why a context bridge instead of only the Request field: the task engine
// builds llmrepo.Request values deep inside chain execution where no session
// identity exists, while the session owner (agentservice.Prompt) sits several
// layers above it. Until every construction site threads Request.SessionKey
// explicitly, the key rides the context the same way the request ID does.
// An explicit Request.SessionKey always wins over the context value.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKeyContextKey, key)
}

// SessionKeyFromContext returns the session cache key set by WithSessionKey,
// or "" when the request has no session identity (stateless requests resolve
// randomly, exactly as before).
func SessionKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(sessionKeyContextKey).(string)
	return key
}

// DeriveSessionKey turns an internal session identifier into the opaque key
// used for cache affinity. The key eventually reaches third parties (it is
// the natural input for OpenAI's prompt_cache_key), so the internal session
// UUID must never travel raw — we send a one-way hash instead. A plain
// domain-separated SHA-256 is used rather than an HMAC with a runtime secret
// because the key must stay stable across process restarts (a restart must
// not cold-start every provider cache) and a session UUID already carries
// enough entropy that the hash is not invertible in practice.
func DeriveSessionKey(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("contenox-session-cache-key:" + sessionID))
	return hex.EncodeToString(sum[:])
}

// effectiveSessionKey resolves the cache-affinity key for one request: an
// explicit Request.SessionKey wins; otherwise the key set on the context by
// the session owner applies; "" means "no session" and resolution stays
// random.
func effectiveSessionKey(ctx context.Context, req Request) string {
	if req.SessionKey != "" {
		return req.SessionKey
	}
	return SessionKeyFromContext(ctx)
}

// CacheHints tells a provider where the stable/volatile boundary of a request
// lies so it can place cache breakpoints (anthropic cache_control, bedrock
// cachePoint, gemini cachedContents, openai prompt_cache_key). Providers map
// what they can and ignore the rest — omission changes nothing, and a hint
// may NEVER change what the model sees: only cache metadata differs. Any
// provider that would have to reorder or rewrite content to honor a hint must
// drop the hint instead.
//
// The session affinity key is not part of this struct: it lives on
// Request.SessionKey (and is filled into the provider-facing hints when
// llmrepo translates them onto the chat config in a later slice).
type CacheHints struct {
	// StableSystem asserts the system instruction is byte-stable across the
	// session, i.e. it is safe to place a cache breakpoint after it.
	StableSystem bool
	// StableTools asserts the tool list (order and encoding) is byte-stable
	// across the session.
	StableTools bool
	// StableHistoryLen is the number of leading history messages the engine
	// asserts are unchanged since the previous request of this session
	// (0 = no assertion). Providers with explicit breakpoints may mark the
	// last of these.
	StableHistoryLen int
	// TTL is an advisory cache lifetime ("5m", "1h") for providers with
	// explicit TTLs. Empty means the provider default.
	TTL string
}

// providerCacheHints translates llmrepo's engine-side CacheHints into the
// provider-facing modelrepo.CacheHints at the canonical-request seam, filling
// the provider cache key from the resolved session key.
//
// PRODUCER (until the task engine emits rich per-call hints): when the caller
// supplied no hints, llmrepo synthesizes conservative defaults on every chat
// and stream request — StableSystem and StableTools are asserted because the
// post-canonicalization request shape makes both byte-stable across a session
// (day-granular {{now}}, canonical tool ordering), and StableHistoryLen stays
// 0 because only the trim logic knows the stable history prefix. This
// activates provider caching with zero caller changes; richer per-call hints
// (history prefix length from the chunked trim) are the deferred taskengine
// follow-up.
func providerCacheHints(hints *CacheHints, sessionKey string) libmodelprovider.CacheHints {
	out := libmodelprovider.CacheHints{
		SessionKey:   sessionKey,
		StableSystem: true,
		StableTools:  true,
	}
	if hints == nil {
		return out
	}
	out.StableSystem = hints.StableSystem
	out.StableTools = hints.StableTools
	out.StableHistoryLen = hints.StableHistoryLen
	if hints.TTL != "" {
		// The TTL is advisory; an unparseable value degrades to the provider
		// default rather than failing the request.
		if d, err := time.ParseDuration(hints.TTL); err == nil && d > 0 {
			out.TTL = d
		}
	}
	return out
}

// canonicalToolOrder is appended by llmrepo as the LAST ChatArgument on every
// chat and stream request. Provider prefix caches key on the exact serialized
// request bytes, and tools render before everything else on most providers —
// a wobbling tool order is a full cache invalidation (blueprint E7). Sorting
// here, at the one seam every request passes through, makes registry and
// allowlist enumeration order irrelevant. Sorting is metadata-only from the
// model's perspective: the same tools are advertised, just in a stable order.
type canonicalToolOrder struct{}

func (canonicalToolOrder) Apply(cfg *libmodelprovider.ChatConfig) {
	sortToolsCanonically(cfg.Tools)
}

// sortToolsCanonically orders tools by (type, qualified function name),
// stably, in place. Stable sort keeps the relative order of pathological
// duplicates deterministic too.
func sortToolsCanonically(tools []libmodelprovider.Tool) {
	sort.SliceStable(tools, func(i, j int) bool {
		return toolSortKey(tools[i]) < toolSortKey(tools[j])
	})
}

func toolSortKey(t libmodelprovider.Tool) string {
	name := ""
	if t.Function != nil {
		name = t.Function.Name
	}
	// NUL separator so ("a", "b.c") can never collide with ("a\x00b", ".c").
	return t.Type + "\x00" + name
}

// withCanonicalRequestShape returns the chat arguments extended with the
// cache-hint declaration and the canonical-tool-order pass. It always copies:
// the variadic slice may share its backing array with the caller, and
// appending in place could clobber the caller's memory. Hints are applied
// before tool sorting so the tool list every adapter caches against is the
// canonical one.
func withCanonicalRequestShape(opts []libmodelprovider.ChatArgument, hints libmodelprovider.CacheHints) []libmodelprovider.ChatArgument {
	out := make([]libmodelprovider.ChatArgument, 0, len(opts)+2)
	out = append(out, opts...)
	out = append(out, libmodelprovider.WithCacheHints(hints))
	out = append(out, canonicalToolOrder{})
	return out
}
