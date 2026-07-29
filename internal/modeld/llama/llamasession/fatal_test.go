//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/internal/transport"
)

// bareSession returns a session with no native state (lctx nil), which is
// enough to exercise the fatal state machine: closeLocked tolerates nil
// native handles, so no llama.cpp runtime is required.
func bareSession() *session {
	return &session{numCtx: 8, plannerCtx: 8}
}

func TestMarkFatalPoisonsSession(t *testing.T) {
	s := bareSession()
	err := s.markFatalLocked(errors.New("boom"))
	if !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("markFatalLocked error = %v, want ErrSessionFatal", err)
	}
	if !s.closed {
		t.Fatal("markFatalLocked did not close the session")
	}
	report := s.ExplainContext()
	if report.FatalError == "" || report.FatalError != s.fatalErr {
		t.Fatalf("ExplainContext FatalError = %q, want recorded cause %q", report.FatalError, s.fatalErr)
	}
	if !report.Closed {
		t.Fatal("ExplainContext does not report the session closed")
	}
	if report.Residency == nil || report.Residency.Error == "" {
		t.Fatalf("fatal session must surface a residency error, got %+v", report.Residency)
	}
}

func TestFatalSessionRejectsEveryEntryPoint(t *testing.T) {
	s := bareSession()
	_ = s.markFatalLocked(errors.New("boom"))
	ctx := context.Background()

	if _, err := s.EnsurePrefix(ctx, llama.PrefixInput{Text: "x"}); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("EnsurePrefix on fatal session = %v, want ErrSessionFatal", err)
	}
	if _, err := s.PrefillSuffix(ctx, llama.SuffixInput{Text: "x"}); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("PrefillSuffix on fatal session = %v, want ErrSessionFatal", err)
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("Snapshot on fatal session = %v, want ErrSessionFatal", err)
	}
	if err := s.Restore(ctx, llama.SessionSnapshot{}); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("Restore on fatal session = %v, want ErrSessionFatal", err)
	}
	if err := s.EvictRange(ctx, residency.Range{Start: 0, End: 1}); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("EvictRange on fatal session = %v, want ErrSessionFatal", err)
	}
	if err := s.AdmitRange(ctx, residency.Range{Start: 0, End: 1}); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("AdmitRange on fatal session = %v, want ErrSessionFatal", err)
	}
	ch, err := s.Decode(ctx, llama.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode returned setup error: %v", err)
	}
	chunk, ok := <-ch
	if !ok || !errors.Is(chunk.Error, llama.ErrSessionFatal) {
		t.Fatalf("Decode chunk on fatal session = %+v, want ErrSessionFatal", chunk)
	}
}

func TestPlainCloseIsNotFatal(t *testing.T) {
	s := bareSession()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := s.EnsurePrefix(context.Background(), llama.PrefixInput{Text: "x"})
	if !errors.Is(err, llama.ErrSessionClosed) {
		t.Fatalf("EnsurePrefix on closed session = %v, want ErrSessionClosed", err)
	}
	if errors.Is(err, llama.ErrSessionFatal) {
		t.Fatal("plain close must not classify as fatal")
	}
	if report := s.ExplainContext(); report.FatalError != "" {
		t.Fatalf("plain close recorded FatalError %q", report.FatalError)
	}
}

func TestMarkFatalKeepsFirstCause(t *testing.T) {
	s := bareSession()
	_ = s.markFatalLocked(errors.New("first"))
	_ = s.markFatalLocked(errors.New("second"))
	if s.fatalErr != "first" {
		t.Fatalf("fatalErr = %q, want first recorded cause", s.fatalErr)
	}
}

func TestFatalizeRoutesOnlyFatalErrors(t *testing.T) {
	s := bareSession()
	plain := errors.New("transient")
	if err := s.fatalizeLocked(plain); err != plain {
		t.Fatalf("fatalizeLocked(plain) = %v, want pass-through", err)
	}
	if s.closed || s.fatalErr != "" {
		t.Fatal("fatalizeLocked poisoned the session on a non-fatal error")
	}
	wrapped := fmt.Errorf("%w: kv remove failed", llama.ErrSessionFatal)
	if err := s.fatalizeLocked(wrapped); !errors.Is(err, llama.ErrSessionFatal) {
		t.Fatalf("fatalizeLocked(fatal) = %v, want ErrSessionFatal", err)
	}
	if !s.closed || s.fatalErr == "" {
		t.Fatal("fatalizeLocked did not poison the session on a fatal error")
	}
}

// TestDecodeRejectsUnknownStructuredProtocol pins the capability-truth rule
// that an unrecognized structured-output protocol must error — never decode
// unconstrained. Supported protocols (json_schema, llama:json_schema,
// llama:json_schema_tool_calls) constrain via GBNF and are covered by the
// system tests; the rejection happens before any native work, so a bare
// session exercises it.
func TestDecodeRejectsUnknownStructuredProtocol(t *testing.T) {
	s := bareSession()
	ch, err := s.Decode(context.Background(), llama.DecodeConfig{
		MaxTokens:        4,
		StructuredOutput: transport.StructuredOutputConfig{Protocol: "openvino:triggered_tags", Payload: "{}"},
	})
	if err != nil {
		t.Fatalf("Decode returned setup error: %v", err)
	}
	chunk, ok := <-ch
	if !ok || !errors.Is(chunk.Error, llama.ErrUnsupportedFeature) {
		t.Fatalf("Decode chunk = %+v, want ErrUnsupportedFeature", chunk)
	}
	if report := s.ExplainContext(); report.DecodeCalls != 0 {
		t.Fatalf("rejected decode must not count as a decode call, got %d", report.DecodeCalls)
	}
}

// TestJSONSchemaToGrammarProducesGBNF exercises the llama.cpp common
// converter end-to-end: a real schema yields a grammar with a root rule, and
// invalid schema JSON reports an error instead of an empty grammar.
func TestJSONSchemaToGrammarProducesGBNF(t *testing.T) {
	schema := `{"type":"object","properties":{"name":{"type":"string"},"count":{"type":"integer"}},"required":["name"]}`
	grammar, err := llamacppshim.JSONSchemaToGrammar(schema)
	if err != nil {
		t.Fatalf("JSONSchemaToGrammar: %v", err)
	}
	if !strings.Contains(grammar, "root") {
		t.Fatalf("grammar has no root rule:\n%s", grammar)
	}
	if _, err := llamacppshim.JSONSchemaToGrammar("{not json"); err == nil {
		t.Fatal("invalid schema JSON must error")
	}
}

// TestSessionCapabilitiesDelegateToSharedMapping keeps the tagged session on
// the pure mapping that TestUnit_LlamaCapabilitiesVerbatim pins in plain CI.
func TestSessionCapabilitiesDelegateToSharedMapping(t *testing.T) {
	s := bareSession()
	s.coldMaxTokens = 1024
	s.sparseAttention = true
	s.slidingWindowAttentionTokens = 512
	if got, want := s.Capabilities(), capabilitiesFor(true, 512, 1024); got != want {
		t.Fatalf("session capabilities = %+v, want shared mapping %+v", got, want)
	}
}
