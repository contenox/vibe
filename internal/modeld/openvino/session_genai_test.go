package openvino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

// fakeGenAIBackend is a string-prompt GenAI stand-in: it records the prompt it
// is asked to stream and tokenizes by rune count, so the adapter's text mapping
// can be tested without the CGO OpenVINO backend.
type fakeGenAIBackend struct {
	streamPrompts               []string
	streamTokenPrompts          [][]int
	streamOptions               []ovsession.GenerateOptions
	generatePrompts             []string
	generateTokenPrompts        [][]int
	generateOptions             []ovsession.GenerateOptions
	prefillTokenPrompts         [][]int
	prefillErr                  error
	templateCalls               [][]ovsession.ChatMessage
	templateTools               []string
	templateAddGenerationPrompt []bool
	templateEnableThinking      []*bool
	templateReasoningEffort     []string
	generateResult              ovsession.GenAIResult
	generateErr                 error
	templateErr                 error
	emit                        []string
	emitChunks                  []ovsession.StreamChunk
	closed                      bool
	supportsColdKV              bool
	exportedColdKV              []ovsession.ColdKVRange
	importedColdKV              []ovsession.ColdKVRange
	importedKV                  [][]byte
}

func (f *fakeGenAIBackend) Generate(_ context.Context, prompt string, opts ovsession.GenerateOptions) (ovsession.GenAIResult, error) {
	f.generatePrompts = append(f.generatePrompts, prompt)
	f.generateOptions = append(f.generateOptions, opts)
	return f.generateResult, f.generateErr
}

func (f *fakeGenAIBackend) GenerateTokens(_ context.Context, tokens []int, opts ovsession.GenerateOptions) (ovsession.GenAIResult, error) {
	f.generateTokenPrompts = append(f.generateTokenPrompts, append([]int(nil), tokens...))
	f.generateOptions = append(f.generateOptions, opts)
	return f.generateResult, f.generateErr
}

func (f *fakeGenAIBackend) Stream(_ context.Context, prompt string, opts ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error) {
	f.streamPrompts = append(f.streamPrompts, prompt)
	f.streamOptions = append(f.streamOptions, opts)
	return f.streamChunks(opts)
}

func (f *fakeGenAIBackend) StreamTokens(_ context.Context, tokens []int, opts ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error) {
	f.streamTokenPrompts = append(f.streamTokenPrompts, append([]int(nil), tokens...))
	f.streamOptions = append(f.streamOptions, opts)
	return f.streamChunks(opts)
}

func (f *fakeGenAIBackend) PrefillTokens(_ context.Context, tokens []int) error {
	f.prefillTokenPrompts = append(f.prefillTokenPrompts, append([]int(nil), tokens...))
	if f.prefillErr != nil {
		return f.prefillErr
	}
	return nil
}

func (f *fakeGenAIBackend) streamChunks(_ ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error) {
	size := len(f.emit)
	if len(f.emitChunks) > 0 {
		size = len(f.emitChunks)
	}
	ch := make(chan ovsession.StreamChunk, size)
	if len(f.emitChunks) > 0 {
		for _, chunk := range f.emitChunks {
			ch <- chunk
		}
	} else {
		for _, t := range f.emit {
			ch <- ovsession.StreamChunk{Text: t}
		}
	}
	close(ch)
	return ch, nil
}

func (f *fakeGenAIBackend) Tokenize(_ context.Context, prompt string, _ bool) ([]int, error) {
	out := make([]int, 0, len([]rune(prompt)))
	for _, r := range prompt {
		out = append(out, int(r))
	}
	return out, nil
}

func (f *fakeGenAIBackend) ApplyChatTemplate(messages []ovsession.ChatMessage, tools string) (string, error) {
	return f.ApplyChatTemplateWithPrompt(messages, tools, true)
}

func (f *fakeGenAIBackend) ApplyChatTemplateWithPrompt(messages []ovsession.ChatMessage, tools string, addGenerationPrompt bool) (string, error) {
	return f.ApplyChatTemplateWithOptions(messages, tools, addGenerationPrompt, nil, "")
}

func (f *fakeGenAIBackend) ApplyChatTemplateWithOptions(messages []ovsession.ChatMessage, tools string, addGenerationPrompt bool, enableThinking *bool, reasoningEffort string) (string, error) {
	cp := append([]ovsession.ChatMessage(nil), messages...)
	f.templateCalls = append(f.templateCalls, cp)
	f.templateTools = append(f.templateTools, tools)
	f.templateAddGenerationPrompt = append(f.templateAddGenerationPrompt, addGenerationPrompt)
	f.templateEnableThinking = append(f.templateEnableThinking, enableThinking)
	f.templateReasoningEffort = append(f.templateReasoningEffort, reasoningEffort)
	if f.templateErr != nil {
		return "", f.templateErr
	}
	out := ""
	for _, m := range messages {
		out += "<|" + m.Role + "|>" + m.Content
	}
	if addGenerationPrompt {
		out += "<|assistant|>"
	}
	return out, nil
}

func (f *fakeGenAIBackend) Close() error { f.closed = true; return nil }

func (f *fakeGenAIBackend) SupportsColdKV() bool { return f.supportsColdKV }

func (f *fakeGenAIBackend) ExportColdKV(_ context.Context, r ovsession.ColdKVRange) ([]byte, error) {
	f.exportedColdKV = append(f.exportedColdKV, cloneColdKVRange(r))
	return append([]byte("openvino-kv:"), byte(r.End-r.Start)), nil
}

func (f *fakeGenAIBackend) ImportColdKV(_ context.Context, r ovsession.ColdKVRange, kv []byte) error {
	f.importedColdKV = append(f.importedColdKV, cloneColdKVRange(r))
	f.importedKV = append(f.importedKV, append([]byte(nil), kv...))
	return nil
}

func cloneColdKVRange(r ovsession.ColdKVRange) ovsession.ColdKVRange {
	r.Tokens = append([]int(nil), r.Tokens...)
	r.PrefixTokens = append([]int(nil), r.PrefixTokens...)
	return r
}

func runesFromInts(tokens []int) []rune {
	out := make([]rune, len(tokens))
	for i, tok := range tokens {
		out[i] = rune(tok)
	}
	return out
}

func ovManifest(stableHash, runtimeDigest string) contextasm.ContextManifest {
	return contextasm.ContextManifest{
		Backend:              "openvino",
		ModelDigest:          "model-d1",
		PromptFormat:         "openvino_chat_template",
		PromptTemplateDigest: "model-d1",
		RuntimeDigest:        runtimeDigest,
		StableByteHash:       stableHash,
	}
}

func TestGenaiSessionDecodePreservesToolHistoryForChatTemplate(t *testing.T) {
	fake := &fakeGenAIBackend{emit: []string{"ok"}}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	fullText := "rulesaskcallingresult"
	stable := "rules"
	m := ovManifest(contextasm.HashString(stable), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: 5, ByteHash: contextasm.HashString("rules")},
		{Kind: "user", ByteStart: 5, ByteEnd: 8, ByteHash: contextasm.HashString("ask")},
		{Kind: "assistant", ByteStart: 8, ByteEnd: 15, ByteHash: contextasm.HashString("calling"), ToolCallsJSON: `[{"id":"call_123","type":"function","function":{"name":"lookup","arguments":"{}"}}]`},
		{Kind: "tool", ByteStart: 15, ByteEnd: len(fullText), ByteHash: contextasm.HashString("result"), ToolCallID: "call_123"},
	}

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Tools: `[{"type":"function"}]`, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: strings.TrimPrefix(fullText, stable), Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for range ch {
	}

	fullCall := -1
	for i, add := range fake.templateAddGenerationPrompt {
		if add && len(fake.templateCalls[i]) == 4 {
			fullCall = i
			break
		}
	}
	if fullCall < 0 {
		t.Fatalf("template calls = %+v add_generation_prompt=%+v, want full decode prompt render", fake.templateCalls, fake.templateAddGenerationPrompt)
	}
	msgs := fake.templateCalls[fullCall]
	if len(msgs) != 4 {
		t.Fatalf("template messages = %+v, want 4", msgs)
	}
	if msgs[2].Role != "assistant" || !strings.Contains(msgs[2].ToolCalls, "call_123") {
		t.Fatalf("assistant tool_calls not preserved: %+v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_123" || msgs[3].Content != "result" {
		t.Fatalf("tool result metadata not preserved: %+v", msgs[3])
	}
	if got := fake.templateTools[fullCall]; got != `[{"type":"function"}]` {
		t.Fatalf("template tools = %+v", fake.templateTools)
	}
	wantPrompt := "<|system|>rules<|user|>ask<|assistant|>calling<|tool|>result<|assistant|>"
	if len(fake.streamTokenPrompts) != 1 || string(runesFromInts(fake.streamTokenPrompts[0])) != wantPrompt {
		t.Fatalf("decode token prompt = %+v, want %q", fake.streamTokenPrompts, wantPrompt)
	}
}

func TestGenaiSessionDecodeConcatenatesStableAndSuffix(t *testing.T) {
	fake := &fakeGenAIBackend{emit: []string{"hel", "lo"}}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "SYSTEM", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{MaxTokens: 8})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var out string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		out += chunk.Text
	}
	if out != "hello" {
		t.Errorf("decoded text = %q, want %q", out, "hello")
	}
	if len(fake.streamTokenPrompts) != 1 || string(runesFromInts(fake.streamTokenPrompts[0])) != "SYSTEMUSER" {
		t.Errorf("streamed token prompt = %v, want one prompt %q", fake.streamTokenPrompts, "SYSTEMUSER")
	}
}

func TestGenaiSessionDecodeNoResidentChatTemplateFailureIsError(t *testing.T) {
	fake := &fakeGenAIBackend{templateErr: errors.New("template exploded")}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest(contextasm.HashString("SYSTEM"), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len("SYSTEM"), ByteHash: contextasm.HashString("SYSTEM")},
	}
	if err := s.Restore(ctx, transport.SessionSnapshot{
		ResidentTokens: 0,
		PrefixTokens:   0,
		StableText:     "SYSTEM",
		PrefixText:     "SYSTEM",
		Manifest:       m,
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	ch, err := s.Decode(ctx, transport.DecodeConfig{MaxTokens: 1})
	if err == nil {
		if ch != nil {
			for range ch {
			}
		}
		t.Fatal("Decode error = nil, want chat template failure")
	}
	if !strings.Contains(err.Error(), "apply chat template for decode") || !strings.Contains(err.Error(), "template exploded") {
		t.Fatalf("Decode error = %v", err)
	}
}

func TestGenaiSessionPoisonedPrefillMarksSessionFatal(t *testing.T) {
	fake := &fakeGenAIBackend{
		prefillErr: errors.New("Check 'm_ref_count > 0' failed; BlockAllocator leaked blocks. Expected num free blocks: 2665, actual: 17"),
	}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()

	_, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "SYSTEM", Manifest: ovManifest("hash-AAA", "r1")})
	if !errors.Is(err, transport.ErrSessionFatal) {
		t.Fatalf("EnsurePrefix error = %v, want ErrSessionFatal", err)
	}
	if !fake.closed {
		t.Fatal("poisoned prefill should close the backend")
	}
	report := s.ExplainContext()
	if !report.Closed || report.FatalError == "" {
		t.Fatalf("report = %+v, want closed fatal context", report)
	}
	_, err = s.PrefillSuffix(ctx, transport.SuffixInput{Text: "USER", Manifest: ovManifest("hash-AAA", "r1")})
	if !errors.Is(err, transport.ErrSessionFatal) {
		t.Fatalf("PrefillSuffix after fatal = %v, want ErrSessionFatal", err)
	}
}

func TestGenaiSessionDeferredPrefillSkipsPhysicalPrefillWithoutColdStore(t *testing.T) {
	fake := &fakeGenAIBackend{emit: []string{"ok"}}
	s := newGenaiSession(fake, 4096)
	s.deferPhysicalPrefill = true
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "SYSTEM", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	if len(fake.prefillTokenPrompts) != 0 {
		t.Fatalf("physical prefill calls = %+v, want none", fake.prefillTokenPrompts)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode error: %v", chunk.Error)
		}
	}
	if len(fake.streamTokenPrompts) != 1 || string(runesFromInts(fake.streamTokenPrompts[0])) != "SYSTEMUSER" {
		t.Fatalf("decode token prompt = %+v, want SYSTEMUSER", fake.streamTokenPrompts)
	}
	report := s.ExplainContext()
	if report.PhysicalPrefillCalls != 0 || report.DeferredPrefillCalls != 2 {
		t.Fatalf("prefill counters = physical %d deferred %d, want 0/2", report.PhysicalPrefillCalls, report.DeferredPrefillCalls)
	}
	if report.DecodeCalls != 1 || report.DecodePromptTokens != len("SYSTEMUSER") {
		t.Fatalf("decode counters = calls %d tokens %d, want 1/%d", report.DecodeCalls, report.DecodePromptTokens, len("SYSTEMUSER"))
	}
}

func TestGenaiSessionDeferredPrefillStillPrefillsWithColdStore(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 6, 10, true)
	s.deferPhysicalPrefill = true
	ctx := context.Background()
	m := ovManifest(contextasm.HashString("abcdef"), "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "abcdef", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if len(fake.prefillTokenPrompts) != 1 {
		t.Fatalf("physical prefill calls = %+v, want one cold-store prefill", fake.prefillTokenPrompts)
	}
	report := s.ExplainContext()
	if report.PhysicalPrefillCalls != 1 || report.DeferredPrefillCalls != 0 {
		t.Fatalf("prefill counters = physical %d deferred %d, want 1/0", report.PhysicalPrefillCalls, report.DeferredPrefillCalls)
	}
}

func TestGenaiSessionExplainContextSurfacesResidencyPlan(t *testing.T) {
	fake := &fakeGenAIBackend{emit: []string{"ok"}}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	stable := "rules"
	fullText := "rulesask"
	m := ovManifest(contextasm.HashString(stable), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: 5, ByteHash: contextasm.HashString("rules")},
		{Kind: "user", ByteStart: 5, ByteEnd: 8, ByteHash: contextasm.HashString("ask")},
	}

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: strings.TrimPrefix(fullText, stable), Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	report := s.ExplainContext()
	res := report.Residency
	if res == nil {
		t.Fatal("ExplainContext did not surface a residency report")
	}
	if res.BudgetTokens != 4096 {
		t.Fatalf("residency budget = %d, want numCtx 4096", res.BudgetTokens)
	}
	if res.TotalTokens != report.ResidentTokens {
		t.Fatalf("residency total = %d, want resident %d", res.TotalTokens, report.ResidentTokens)
	}
	if res.ColdTokens != 0 || res.OverBudget {
		t.Fatalf("expected all-hot under the numCtx budget: %+v", res)
	}
	if res.HotBlocks == 0 || res.ProtectedTokens == 0 {
		t.Fatalf("expected pinned hot blocks, got %+v", res)
	}
	// OpenVINO without a cold store does residency declaratively: no
	// runtime-driven KV range surgery, but it runs XAttention sparse attention
	// natively and can always recompute a range by re-prefilling from tokens.
	if want := (transport.ResidencyCapabilities{SparseAttention: true, RecomputeRange: true}); res.Capabilities != want {
		t.Fatalf("openvino capabilities = %+v, want %+v", res.Capabilities, want)
	}
	if res.Error != "" {
		t.Fatalf("unexpected residency error: %q", res.Error)
	}
}

func TestGenaiSessionCapabilitiesReflectConfiguredSparseAttention(t *testing.T) {
	s := newGenaiSessionWithNativeFeatures(&fakeGenAIBackend{}, 4096, 4096, false, false, 0)
	if caps := s.Capabilities(); caps.SparseAttention {
		t.Fatalf("SparseAttention = true, want false when native sparse attention is disabled: %+v", caps)
	}
}

func TestGenaiSessionCapabilitiesReportSlidingWindowAttention(t *testing.T) {
	s := newGenaiSessionWithNativeFeatures(&fakeGenAIBackend{}, 4096, 4096, false, true, 512)
	if caps := s.Capabilities(); caps.SlidingWindowAttentionTokens != 512 {
		t.Fatalf("SlidingWindowAttentionTokens = %d, want 512: %+v", caps.SlidingWindowAttentionTokens, caps)
	}
}

func TestGenaiSessionColdStoreEvictAdmitUsesBackendKVHooks(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 6, 10, true)
	ctx := context.Background()
	m := ovManifest(contextasm.HashString("abcdef"), "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "abcdef", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	report := s.ExplainContext()
	if report.Residency == nil {
		t.Fatal("ExplainContext did not surface a residency report")
	}
	// Capabilities report ability: with the lossless cold path active the
	// session can do native KV surgery AND can still recompute from its token
	// tape. That admits physically use the KV hooks (not recompute) is asserted
	// below on the fake backend's export/import calls.
	wantCaps := transport.ResidencyCapabilities{
		RemoveTail:      true,
		RemoveMiddle:    true,
		PositionShift:   true,
		SparseAttention: true,
		ColdStore:       true,
		RecomputeRange:  true,
	}
	if report.Residency.Capabilities != wantCaps {
		t.Fatalf("capabilities = %+v, want %+v", report.Residency.Capabilities, wantCaps)
	}

	exec := any(s).(residency.Executor)
	r := residency.Range{Start: 2, End: 4}
	if err := exec.EvictRange(ctx, r); err != nil {
		t.Fatalf("EvictRange: %v", err)
	}
	if len(fake.exportedColdKV) != 1 {
		t.Fatalf("export calls = %d, want 1", len(fake.exportedColdKV))
	}
	if got := fake.exportedColdKV[0]; got.Start != 2 || got.End != 4 || string([]rune{rune(got.Tokens[0]), rune(got.Tokens[1])}) != "cd" || string(runesFromInts(got.PrefixTokens)) != "abcdef" || got.TokenHash == "" {
		t.Fatalf("export range = %+v, want cd [2,4) with abcdef prefix and hash", got)
	}
	if got := s.ExplainContext(); got.ResidentTokens != 4 || got.PrefixTokens != 4 {
		t.Fatalf("after evict context = %+v, want resident=4 prefix=4", got)
	}

	if err := exec.AdmitRange(ctx, r); err != nil {
		t.Fatalf("AdmitRange: %v", err)
	}
	if len(fake.importedColdKV) != 1 {
		t.Fatalf("shifted import calls = %d, want 1", len(fake.importedColdKV))
	}
	if got := fake.importedColdKV[0]; got.Start != 2 || got.End != 4 || got.DestStart != 4 || string(runesFromInts(got.Tokens)) != "cd" || string(runesFromInts(got.PrefixTokens)) != "abefcd" || got.TokenHash == "" {
		t.Fatalf("shifted import range = %+v, want cd [2,4) -> 4 with abefcd prefix and hash", got)
	}
	if got := s.ExplainContext(); got.ResidentTokens != 6 {
		t.Fatalf("after admit context = %+v, want resident=6", got)
	}
	if len(fake.prefillTokenPrompts) != 2 {
		t.Fatalf("prefill calls = %+v, want ensure/evict only", fake.prefillTokenPrompts)
	}
	if got := string(runesFromInts(fake.prefillTokenPrompts[len(fake.prefillTokenPrompts)-1])); got != "abef" {
		t.Fatalf("last prefill prompt after shifted import = %q, want abef", got)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode after admit: %v", err)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode after admit error: %v", chunk.Error)
		}
	}
	if len(fake.streamTokenPrompts) != 1 || string(runesFromInts(fake.streamTokenPrompts[0])) != "abefcd" {
		t.Fatalf("decode token prompt after shifted admit = %+v, want abefcd", fake.streamTokenPrompts)
	}
}

func TestGenaiSessionColdStoreTailAdmitUsesNativeImport(t *testing.T) {
	fake := &fakeGenAIBackend{supportsColdKV: true}
	s := newGenaiSessionWithPlanner(fake, 6, 10, true)
	ctx := context.Background()
	m := ovManifest(contextasm.HashString("abcdef"), "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "abcdef", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	r := residency.Range{Start: 4, End: 6}
	if err := s.EvictRange(ctx, r); err != nil {
		t.Fatalf("EvictRange: %v", err)
	}
	if err := s.AdmitRange(ctx, r); err != nil {
		t.Fatalf("AdmitRange: %v", err)
	}
	if len(fake.importedColdKV) != 1 {
		t.Fatalf("import calls = %d, want 1", len(fake.importedColdKV))
	}
	if got := fake.importedColdKV[0]; got.Start != 4 || got.End != 6 || got.DestStart != 4 || string(runesFromInts(got.Tokens)) != "ef" || got.TokenHash == "" {
		t.Fatalf("import range = %+v, want ef [4,6) -> 4 with hash", got)
	}
	if got := string(runesFromInts(fake.importedColdKV[0].PrefixTokens)); got != "abcdef" {
		t.Fatalf("tail import prefix = %q, want abcdef", got)
	}
	if len(fake.importedKV) != 1 || len(fake.importedKV[0]) == 0 {
		t.Fatalf("imported kv payload = %+v, want bytes", fake.importedKV)
	}
	if len(fake.prefillTokenPrompts) != 2 {
		t.Fatalf("tail prefill calls = %+v, want ensure/evict only", fake.prefillTokenPrompts)
	}
	if got := string(runesFromInts(fake.prefillTokenPrompts[len(fake.prefillTokenPrompts)-1])); got != "abcd" {
		t.Fatalf("last tail prefill prompt = %q, want abcd", got)
	}
}

func TestGenaiSessionColdStoreDisabledWithoutBackendHooks(t *testing.T) {
	s := newGenaiSessionWithPlanner(&fakeGenAIBackend{}, 6, 10, true)
	ctx := context.Background()
	m := ovManifest(contextasm.HashString("abcdef"), "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "abcdef", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	report := s.ExplainContext()
	if report.Residency == nil || report.Residency.Capabilities.ColdStore {
		t.Fatalf("cold-store capability should be disabled without hooks: %+v", report.Residency)
	}
	err := s.EvictRange(ctx, residency.Range{Start: 2, End: 4})
	if !errors.Is(err, transport.ErrUnsupportedFeature) {
		t.Fatalf("EvictRange without hooks = %v, want ErrUnsupportedFeature", err)
	}
}

func TestGenaiSessionEvictionEnabledGeneratesPastNumCtx(t *testing.T) {
	ctx := context.Background()
	m := ovManifest("h", "r1")
	longSuffix := "this is a long user turn that exceeds the tiny window"

	// Eviction ON: the pipeline bounds physical KV by evicting, so prefill and
	// decode past numCtx must not overflow.
	on := newGenaiSessionWithEviction(&fakeGenAIBackend{emit: []string{"ok"}}, 8, true)
	if _, err := on.EnsurePrefix(ctx, transport.PrefixInput{Text: "sys", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := on.PrefillSuffix(ctx, transport.SuffixInput{Text: longSuffix, Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix past numCtx with eviction should not overflow: %v", err)
	}
	ch, err := on.Decode(ctx, transport.DecodeConfig{MaxTokens: 4})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for c := range ch {
		if c.Error != nil {
			t.Fatalf("decode past numCtx with eviction should not overflow: %v", c.Error)
		}
	}

	// Eviction OFF: the same input is a hard overflow (gating works).
	off := newGenaiSession(&fakeGenAIBackend{}, 8)
	if _, err := off.EnsurePrefix(ctx, transport.PrefixInput{Text: "sys", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := off.PrefillSuffix(ctx, transport.SuffixInput{Text: longSuffix, Manifest: m}); !errors.Is(err, transport.ErrContextOverflow) {
		t.Fatalf("PrefillSuffix without eviction = %v, want ErrContextOverflow", err)
	}
}

func TestGenaiSessionEnsurePrefixUsesTokenLCPReuse(t *testing.T) {
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	ctx := context.Background()

	first, err := s.EnsurePrefix(ctx, transport.PrefixInput{
		Text:     "abcdef",
		Manifest: ovManifest(contextasm.HashString("abcdef"), "r1"),
	})
	if err != nil {
		t.Fatalf("EnsurePrefix first: %v", err)
	}
	if first.ReusedTokens != 0 || first.PrefilledTokens != 6 || first.DroppedTokens != 0 {
		t.Fatalf("first prefix status = %+v, want cold 6-token prefill", first)
	}

	second, err := s.EnsurePrefix(ctx, transport.PrefixInput{
		Text:     "abcXYZ",
		Manifest: ovManifest(contextasm.HashString("abcXYZ"), "r1"),
	})
	if err != nil {
		t.Fatalf("EnsurePrefix second: %v", err)
	}
	if second.ReusedTokens != 3 || second.PrefilledTokens != 3 || second.DroppedTokens != 3 {
		t.Fatalf("second prefix status = %+v, want token LCP reuse=3 prefilled=3 dropped=3", second)
	}
	if second.StableTokenHash == "" {
		t.Fatal("StableTokenHash was not backend-tokenizer populated")
	}
}

func TestGenaiSessionPrefillSuffixAppliesModelChatTemplate(t *testing.T) {
	fake := &fakeGenAIBackend{}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	fullText := "rulesask"
	stable := "rules"
	m := ovManifest(contextasm.HashString(stable), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: contextasm.HashString(stable)},
		{Kind: "user", ByteStart: len(stable), ByteEnd: len(fullText), ByteHash: contextasm.HashString("ask")},
	}

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	st, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: strings.TrimPrefix(fullText, stable), Manifest: m})
	if err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	wantSuffix := "<|user|>ask<|assistant|>"
	if st.SuffixTokens != len([]rune(wantSuffix)) {
		t.Fatalf("SuffixTokens = %d, want templated suffix token count %d", st.SuffixTokens, len([]rune(wantSuffix)))
	}
	fullCall := -1
	for i, add := range fake.templateAddGenerationPrompt {
		if add && len(fake.templateCalls[i]) == 2 {
			fullCall = i
			break
		}
	}
	if fullCall < 0 {
		t.Fatalf("template calls = %+v add_generation_prompt=%+v, want full prompt template call", fake.templateCalls, fake.templateAddGenerationPrompt)
	}
	msgs := fake.templateCalls[fullCall]
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("template messages = %+v, want system and user", msgs)
	}
}

func TestGenaiSessionPopulatesManifestTokenRangesAndResidencyPlan(t *testing.T) {
	fake := &fakeGenAIBackend{}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	stable := "rules"
	suffix := "ask"
	fullText := stable + suffix
	m := ovManifest(contextasm.HashString(stable), "r1")
	m.Segments = []contextasm.ManifestSegment{
		{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: contextasm.HashString(stable)},
		{Kind: "user", ByteStart: len(stable), ByteEnd: len(fullText), ByteHash: contextasm.HashString(suffix)},
	}

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: stable, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: suffix, Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}

	report := s.ExplainContext()
	if report.Manifest.StableTokenHash == "" || report.Manifest.VolatileTokenHash == "" {
		t.Fatalf("manifest token hashes not populated: %+v", report.Manifest)
	}
	if len(report.Manifest.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(report.Manifest.Segments))
	}
	stablePrompt := "<|system|>rules"
	fullPrompt := stablePrompt + "<|user|>ask<|assistant|>"
	if got := report.Manifest.Segments[0]; got.TokenStart != 0 || got.TokenEnd != len(stablePrompt) || got.TokenHash == "" {
		t.Fatalf("stable token range = %+v, want [0,%d)", got, len(stablePrompt))
	}
	if got := report.Manifest.Segments[1]; got.TokenStart != len(stablePrompt) || got.TokenEnd != len(fullPrompt) || got.TokenHash == "" {
		t.Fatalf("volatile token range = %+v, want [%d,%d)", got, len(stablePrompt), len(fullPrompt))
	}
	if s.residencyErr != "" {
		t.Fatalf("residency error = %q", s.residencyErr)
	}
	if s.residencyPlan.TotalTokens != len(fullPrompt) || len(s.residencyPlan.KeepHot) != 2 || len(s.residencyPlan.EvictCold) != 0 {
		t.Fatalf("residency plan = %+v, want both blocks hot", s.residencyPlan)
	}
}

func TestGenaiSessionDecodePassesParserProtocolsAndThinking(t *testing.T) {
	fake := &fakeGenAIBackend{emitChunks: []ovsession.StreamChunk{
		{Thinking: "reason "},
		{Text: "answer"},
	}}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	seed := 7
	ch, err := s.Decode(ctx, transport.DecodeConfig{
		MaxTokens:       8,
		TopK:            11,
		Seed:            &seed,
		ParserProtocols: []string{"openvino:deepseek_r1_reasoning_incremental_parser"},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var text, thinking string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		text += chunk.Text
		thinking += chunk.Thinking
	}
	if text != "answer" || thinking != "reason " {
		t.Fatalf("decoded text/thinking = %q/%q, want answer/reason", text, thinking)
	}
	if len(fake.streamOptions) != 1 {
		t.Fatalf("stream options = %d, want 1", len(fake.streamOptions))
	}
	if got := fake.streamOptions[0].ParserProtocols; len(got) != 1 || got[0] != "openvino:deepseek_r1_reasoning_incremental_parser" {
		t.Fatalf("parser protocols = %+v", got)
	}
	if fake.streamOptions[0].TopK == nil || *fake.streamOptions[0].TopK != 11 || fake.streamOptions[0].Seed == nil || *fake.streamOptions[0].Seed != 7 {
		t.Fatalf("stream options = %+v, want TopK=11 Seed=7", fake.streamOptions[0])
	}
}

func TestGenaiSessionDecodeCompleteParserEmitsParsedToolCalls(t *testing.T) {
	fake := &fakeGenAIBackend{
		generateResult: ovsession.GenAIResult{
			Text:       "raw model output",
			ParsedJSON: `{"content":"preface","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup_weather","arguments":{"city":"Berlin"}}}]}`,
		},
	}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{
		MaxTokens:       8,
		ParserProtocols: []string{"openvino:llama3_json_tool_parser"},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var chunks []transport.StreamChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode error: %v", chunk.Error)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Text != "preface" {
		t.Fatalf("text = %q, want parsed content", chunks[0].Text)
	}
	if len(chunks[0].ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", chunks[0].ToolCalls)
	}
	call := chunks[0].ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "lookup_weather" || call.Function.Arguments != `{"city":"Berlin"}` {
		t.Fatalf("tool call = %+v", call)
	}
	lastGenerate := fake.generateOptions[len(fake.generateOptions)-1]
	if len(lastGenerate.ParserProtocols) != 1 || lastGenerate.ParserProtocols[0] != "openvino:llama3_json_tool_parser" {
		t.Fatalf("generate options = %+v", fake.generateOptions)
	}
	if len(fake.generateTokenPrompts) != 1 || string(runesFromInts(fake.generateTokenPrompts[0])) != "USER" {
		t.Fatalf("complete parser should use GenerateTokens with prompt USER, got %+v", fake.generateTokenPrompts)
	}
	if len(fake.streamTokenPrompts) != 0 {
		t.Fatalf("complete parser should not use StreamTokens, got stream prompts %+v", fake.streamTokenPrompts)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaToolCalls(t *testing.T) {
	fake := &fakeGenAIBackend{
		generateResult: ovsession.GenAIResult{
			Text: `{"tool_calls":[{"function":{"name":"echo.echo","arguments":{"input":"hello"}}}]}`,
		},
	}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{
		MaxTokens: 8,
		StructuredOutput: transport.StructuredOutputConfig{
			Protocol: "openvino:json_schema_tool_calls",
			Payload:  `{"type":"object"}`,
		},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var chunks []transport.StreamChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode error: %v", chunk.Error)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || len(chunks[0].ToolCalls) != 1 {
		t.Fatalf("chunks = %+v, want one parsed tool call", chunks)
	}
	call := chunks[0].ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "echo.echo" || call.Function.Arguments != `{"input":"hello"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if len(fake.generateOptions) == 0 {
		t.Fatalf("generate options = %+v", fake.generateOptions)
	}
	lastGenerate := fake.generateOptions[len(fake.generateOptions)-1]
	if got := lastGenerate.StructuredOutput; got.Protocol != "openvino:triggered_tags" || got.Payload != `{"type":"object"}` {
		t.Fatalf("structured output = %+v", got)
	}
	if len(fake.generateTokenPrompts) != 1 || string(runesFromInts(fake.generateTokenPrompts[0])) != "USER" {
		t.Fatalf("structured output should use GenerateTokens with prompt USER, got %+v", fake.generateTokenPrompts)
	}
	if len(fake.streamTokenPrompts) != 0 {
		t.Fatalf("structured output should not use StreamTokens, got stream prompts %+v", fake.streamTokenPrompts)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaAllowsContentOnly(t *testing.T) {
	chunk, err := sessionkit.StructuredToolCallChunk(`{"content":"answer without tools"}`)
	if err != nil {
		t.Fatalf("content-only envelope: %v", err)
	}
	if chunk.Text != "answer without tools" {
		t.Fatalf("text = %q, want content", chunk.Text)
	}
	if len(chunk.ToolCalls) != 0 {
		t.Fatalf("tool calls = %+v, want none", chunk.ToolCalls)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaAllowsPlainText(t *testing.T) {
	chunk, err := sessionkit.StructuredToolCallChunk(`answer without tools`)
	if err != nil {
		t.Fatalf("plain text: %v", err)
	}
	if chunk.Text != "answer without tools" {
		t.Fatalf("text = %q, want plain text", chunk.Text)
	}
	if len(chunk.ToolCalls) != 0 {
		t.Fatalf("tool calls = %+v, want none", chunk.ToolCalls)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaParsesQwenToolCallTag(t *testing.T) {
	chunk, err := sessionkit.StructuredToolCallChunk("prefix\n<tool_call>\n{\"name\":\"echo.echo\",\"arguments\":{\"input\":\"hello\"}}\n</tool_call>\n")
	if err != nil {
		t.Fatalf("qwen tool call tag: %v", err)
	}
	if chunk.Text != "prefix" {
		t.Fatalf("text = %q, want prefix", chunk.Text)
	}
	if len(chunk.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", chunk.ToolCalls)
	}
	call := chunk.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "echo.echo" || call.Function.Arguments != `{"input":"hello"}` {
		t.Fatalf("tool call = %+v", call)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaRejectsMalformedLegacyEnvelope(t *testing.T) {
	_, err := sessionkit.StructuredToolCallChunk(`{"function":{"name":"echo.echo","arguments":{"input":"hello"}}}`)
	if err == nil || !strings.Contains(err.Error(), "contained neither content nor tool_calls") {
		t.Fatalf("single-object fallback error = %v", err)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaRejectsEmptyParsedEnvelope(t *testing.T) {
	_, err := chunkFromGenAIResult(
		ovsession.GenAIResult{ParsedJSON: `{}`},
		transport.StructuredOutputConfig{Protocol: "openvino:json_schema_tool_calls"},
	)
	if err == nil || !strings.Contains(err.Error(), "contained neither content nor tool_calls") {
		t.Fatalf("empty parsed envelope error = %v", err)
	}
}

func TestGenaiSessionDecodeStructuredJSONSchemaRejectsUnsupportedProtocolWithParsedJSON(t *testing.T) {
	_, err := chunkFromGenAIResult(
		ovsession.GenAIResult{ParsedJSON: `{"content":"ignored"}`},
		transport.StructuredOutputConfig{Protocol: "openvino:qwen_xml_parameters"},
	)
	if err == nil || !strings.Contains(err.Error(), `unsupported structured output result protocol "openvino:qwen_xml_parameters"`) {
		t.Fatalf("unsupported parsed protocol error = %v", err)
	}
}

func TestGenaiSessionDecodeRejectsQwenXMLParametersStructuredOutput(t *testing.T) {
	fake := &fakeGenAIBackend{
		generateResult: ovsession.GenAIResult{
			Text: `<parameter=input>hello</parameter><parameter=count>2</parameter>`,
		},
	}
	s := newGenaiSession(fake, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	ch, err := s.Decode(ctx, transport.DecodeConfig{
		MaxTokens: 8,
		StructuredOutput: transport.StructuredOutputConfig{
			Protocol: "openvino:qwen_xml_parameters",
			Payload:  `{"type":"object"}`,
			ToolName: "echo.echo",
		},
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for chunk := range ch {
		if chunk.Error == nil {
			t.Fatalf("chunk error = nil, want unsupported structured output protocol")
		}
		if !strings.Contains(chunk.Error.Error(), `unsupported structured output result protocol "openvino:qwen_xml_parameters"`) {
			t.Fatalf("chunk error = %v", chunk.Error)
		}
		return
	}
	t.Fatal("decode produced no chunk")
}

func TestGenaiSessionWarmReuseReportedOnRepeatedPrefix(t *testing.T) {
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")

	cold, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: m})
	if err != nil {
		t.Fatalf("EnsurePrefix cold: %v", err)
	}
	if cold.ReusedTokens != 0 || cold.PrefilledTokens != len("STABLE") {
		t.Errorf("cold status = %+v, want reused=0 prefilled=%d", cold, len("STABLE"))
	}
	warm, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: m})
	if err != nil {
		t.Fatalf("EnsurePrefix warm: %v", err)
	}
	if warm.ReusedTokens != len("STABLE") || warm.PrefilledTokens != 0 {
		t.Errorf("warm status = %+v, want reused=%d prefilled=0", warm, len("STABLE"))
	}
}

func TestGenaiSessionIncompatibleManifestDropsPrefix(t *testing.T) {
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	ctx := context.Background()

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: ovManifest("hash-AAA", "r1")}); err != nil {
		t.Fatalf("EnsurePrefix first: %v", err)
	}
	// Different runtime digest => incompatible => the resident string prefix must
	// be dropped, not reused.
	got, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: ovManifest("hash-AAA", "r2")})
	if err != nil {
		t.Fatalf("EnsurePrefix second: %v", err)
	}
	if got.ReusedTokens != 0 {
		t.Errorf("reused tokens = %d across an incompatible runtime, want 0", got.ReusedTokens)
	}
	if got.DroppedTokens != len("STABLE") {
		t.Errorf("dropped tokens = %d, want %d", got.DroppedTokens, len("STABLE"))
	}
}

func TestGenaiSessionSuffixManifestMismatchRejected(t *testing.T) {
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	ctx := context.Background()

	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: ovManifest("hash-AAA", "r1")}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	_, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: "USER", Manifest: ovManifest("hash-AAA", "r2")})
	if !errors.Is(err, contextasm.ErrManifestMismatch) {
		t.Fatalf("PrefillSuffix mismatch error = %v, want ErrManifestMismatch", err)
	}
}

func TestGenaiSessionSnapshotRestoreLogicalState(t *testing.T) {
	ctx := context.Background()
	m := ovManifest("hash-AAA", "r1")
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Tools: `[{"type":"function"}]`, Manifest: m}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	if _, err := s.PrefillSuffix(ctx, transport.SuffixInput{Text: "USER", Manifest: m}); err != nil {
		t.Fatalf("PrefillSuffix: %v", err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.State) != 0 {
		t.Fatalf("OpenVINO logical snapshot should not claim opaque KV bytes, got %d", len(snap.State))
	}
	if snap.ResidentTokens != len("STABLEUSER") || snap.PrefixTokens != len("STABLE") || snap.StableText != "STABLE" || snap.PrefixText != "STABLE" {
		t.Fatalf("snapshot did not capture logical state: %+v", snap)
	}

	fake := &fakeGenAIBackend{}
	restored := newGenaiSession(fake, 4096)
	if err := restored.Restore(ctx, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.ExplainContext(); got.ResidentTokens != len("STABLEUSER") || got.PrefixTokens != len("STABLE") || got.ManifestDigest == "" {
		t.Fatalf("restored context = %+v", got)
	}
	ch, err := restored.Decode(ctx, transport.DecodeConfig{MaxTokens: 1})
	if err != nil {
		t.Fatalf("Decode restored: %v", err)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("decode chunk error: %v", chunk.Error)
		}
	}
	if len(fake.streamTokenPrompts) != 1 || string(runesFromInts(fake.streamTokenPrompts[0])) != "STABLEUSER" {
		t.Fatalf("restored token prompt = %v, want STABLEUSER", fake.streamTokenPrompts)
	}
}

func TestGenaiSessionRestoreRejectsIncompatibleRuntime(t *testing.T) {
	ctx := context.Background()
	s := newGenaiSession(&fakeGenAIBackend{}, 4096)
	if _, err := s.EnsurePrefix(ctx, transport.PrefixInput{Text: "STABLE", Manifest: ovManifest("hash-AAA", "r1")}); err != nil {
		t.Fatalf("EnsurePrefix: %v", err)
	}
	err := s.Restore(ctx, transport.SessionSnapshot{
		ResidentTokens: len("STABLE"),
		PrefixTokens:   len("STABLE"),
		NumCtx:         4096,
		StableText:     "STABLE",
		PrefixText:     "STABLE",
		Manifest:       ovManifest("hash-AAA", "r2"),
	})
	if !errors.Is(err, contextasm.ErrManifestMismatch) {
		t.Fatalf("Restore mismatch error = %v, want ErrManifestMismatch", err)
	}
}

func TestGenaiSessionCloseStopsUse(t *testing.T) {
	fake := &fakeGenAIBackend{}
	s := newGenaiSession(fake, 4096)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not reach the backend")
	}
	if _, err := s.EnsurePrefix(context.Background(), transport.PrefixInput{Text: "x"}); !errors.Is(err, transport.ErrSessionClosed) {
		t.Fatalf("EnsurePrefix after close = %v, want ErrSessionClosed", err)
	}
}
