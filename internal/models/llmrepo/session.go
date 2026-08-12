package llmrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	libmodelprovider "github.com/contenox/contenox/internal/models/modelrepo"
)

type sessionKeyContextKeyType struct{}

var sessionKeyContextKey sessionKeyContextKeyType

// WithSessionKey stores an already-derived session cache key on the context; an explicit Request.SessionKey always wins over the context value.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKeyContextKey, key)
}

// SessionKeyFromContext returns the session cache key set by WithSessionKey, or "" when the request has no session identity.
func SessionKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(sessionKeyContextKey).(string)
	return key
}

// DeriveSessionKey turns an internal session identifier into the opaque cache-affinity key so the session ID never travels raw.
func DeriveSessionKey(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("contenox-session-cache-key:" + sessionID))
	return hex.EncodeToString(sum[:])
}

func effectiveSessionKey(ctx context.Context, req Request) string {
	if req.SessionKey != "" {
		return req.SessionKey
	}
	return SessionKeyFromContext(ctx)
}

// CacheHints tells a provider where the stable/volatile boundary of a request lies so it can place cache breakpoints; a hint must never change what the model sees, only cache metadata.
type CacheHints struct {
	// StableSystem asserts the system instruction is byte-stable across the session, i.e. it is safe to place a cache breakpoint after it.
	StableSystem bool
	// StableTools asserts the tool list (order and encoding) is byte-stable across the session.
	StableTools bool
	// StableHistoryLen is the number of leading history messages asserted unchanged since the previous request (0 = no assertion).
	StableHistoryLen int
	// TTL is an advisory cache lifetime (e.g. "5m") for providers with explicit TTLs; empty means the provider default.
	TTL string
}

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
		// unparseable TTL degrades to the provider default rather than failing the request
		if d, err := time.ParseDuration(hints.TTL); err == nil && d > 0 {
			out.TTL = d
		}
	}
	return out
}

type canonicalToolOrder struct{}

func (canonicalToolOrder) Apply(cfg *libmodelprovider.ChatConfig) {
	sortToolsCanonically(cfg.Tools)
}

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

// withCanonicalRequestShape always copies opts since the variadic slice may share its backing array with the caller.
func withCanonicalRequestShape(opts []libmodelprovider.ChatArgument, hints libmodelprovider.CacheHints) []libmodelprovider.ChatArgument {
	out := make([]libmodelprovider.ChatArgument, 0, len(opts)+2)
	out = append(out, opts...)
	out = append(out, libmodelprovider.WithCacheHints(hints))
	out = append(out, canonicalToolOrder{})
	return out
}
