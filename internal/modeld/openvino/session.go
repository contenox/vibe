package openvino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

// genaiBackend is the subset of *ovsession.GenAISession the transport-session
// adapter depends on. Narrowing it to an interface keeps the warm-reuse mapping
// testable without compiling the CGO OpenVINO GenAI backend.
type genaiBackend interface {
	Generate(ctx context.Context, prompt string, opts ovsession.GenerateOptions) (ovsession.GenAIResult, error)
	GenerateTokens(ctx context.Context, tokens []int, opts ovsession.GenerateOptions) (ovsession.GenAIResult, error)
	Stream(ctx context.Context, prompt string, opts ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error)
	StreamTokens(ctx context.Context, tokens []int, opts ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error)
	PrefillTokens(ctx context.Context, tokens []int) error
	Tokenize(ctx context.Context, prompt string, addSpecial bool) ([]int, error)
	// ApplyChatTemplate renders role/content turns with the MODEL's own chat
	// template (held inside the IR tokenizer), producing the prompt string the
	// pipeline expects. This is why generation runs with apply_chat_template=false
	// in the shim: the caller templates first, with the model-native template.
	ApplyChatTemplate(messages []ovsession.ChatMessage, toolsJSON string) (string, error)
	ApplyChatTemplateWithPrompt(messages []ovsession.ChatMessage, toolsJSON string, addGenerationPrompt bool) (string, error)
	// ApplyChatTemplateWithOptions forwards thinking/effort controls as
	// chat-template extra context (enable_thinking / reasoning_effort).
	ApplyChatTemplateWithOptions(messages []ovsession.ChatMessage, toolsJSON string, addGenerationPrompt bool, enableThinking *bool, reasoningEffort string) (string, error)
	Close() error
}

// The native OpenVINO GenAI session is the production backend; the assertion
// holds in every build because the no-CGO stub mirrors its method set.
var _ genaiBackend = (*ovsession.GenAISession)(nil)

type EmbedSessionBackend interface {
	Embed(ctx context.Context, prompt string) ([]float32, error)
	Close() error
}

var _ EmbedSessionBackend = (*ovsession.EmbedSession)(nil)

// genaiSession adapts OpenVINO GenAI to the runtime's transport.Session contract.
//
// OpenVINO GenAI owns the tokenizer, chat template, and physical prefix cache
// inside ContinuousBatchingPipeline. The adapter keeps transport-level token
// accounting over the model-native prompt token sequence, so residency
// operations mutate the same token tape that decode sends back to GenAI.
type genaiSession struct {
	backend       genaiBackend
	numCtx        int
	plannerCtx    int
	coldMaxTokens int
	coldTokens    int
	coldClock     int64
	coldBlocks    map[string]*openvinoColdBlock
	coldRangeKey  map[string]string
	// coldKVLossless is true when the session's KV precision round-trips raw KV
	// bytes exactly. The cold-KV evict/admit/snapshot path copies physical KV
	// blocks, which only survive at float precision (f16/f32); quantized
	// precisions (q8_0/q4_*) dequantize-then-requantize and silently degrade an
	// evicted-then-readmitted block. When false, the cold path is disabled so a
	// lossy round-trip can never happen quietly.
	coldKVLossless bool

	mu         sync.Mutex
	closed     bool
	fatalErr   string
	manifest   transport.ContextManifest
	stable     string // raw stable text from the runtime
	suffix     string // raw volatile text appended after stable
	prefixText string // model-native stable prompt text whose tokens form resident[:prefixLen]
	stableMsgs []ovsession.ChatMessage
	tools      string // model-native tool definitions JSON, rendered via the chat template
	resident   []int  // model-native prompt token IDs resident by contract
	prefixLen  int    // how many resident tokens belong to the stable prefix

	// evictionEnabled is set when the GenAI pipeline runs native cache eviction
	// (sink+recent+evictable). Then numCtx is the physical hot budget the pipeline
	// keeps by evicting, not a hard logical ceiling, so the adapter lets the
	// logical context grow past it instead of returning ErrContextOverflow — the
	// OpenVINO parallel to the llama decode-time slide.
	evictionEnabled              bool
	sparseAttention              bool
	slidingWindowAttentionTokens int

	residencyPlan residency.Plan
	residencyErr  string

	deferPhysicalPrefill   bool
	physicalPrefillCalls   int
	physicalPrefillTokens  int
	deferredPrefillCalls   int
	deferredPrefillTokens  int
	decodeCalls            int
	decodePromptTokenCount int
}

func newGenaiSession(backend genaiBackend, numCtx int) *genaiSession {
	return newGenaiSessionWithEviction(backend, numCtx, false)
}

func newGenaiSessionWithEviction(backend genaiBackend, numCtx int, eviction bool) *genaiSession {
	return newGenaiSessionWithPlanner(backend, numCtx, 0, eviction)
}

func newGenaiSessionWithPlanner(backend genaiBackend, numCtx, plannerCtx int, eviction bool) *genaiSession {
	return newGenaiSessionWithNativeFeatures(backend, numCtx, plannerCtx, eviction, true, 0)
}

func newGenaiSessionWithNativeFeatures(backend genaiBackend, numCtx, plannerCtx int, eviction, sparseAttention bool, slidingWindowAttentionTokens int) *genaiSession {
	if plannerCtx <= 0 {
		plannerCtx = numCtx
	}
	if plannerCtx < numCtx {
		plannerCtx = numCtx
	}
	return &genaiSession{
		backend:                      backend,
		numCtx:                       numCtx,
		plannerCtx:                   plannerCtx,
		coldMaxTokens:                max(plannerCtx-numCtx, 0),
		evictionEnabled:              eviction,
		sparseAttention:              sparseAttention,
		slidingWindowAttentionTokens: slidingWindowAttentionTokens,
		// Default to the lossless (f16) assumption; OpenSession lowers this to the
		// model's actual configured precision. Constructed test sessions keep the
		// cold path enabled, matching prior behavior.
		coldKVLossless: true,
	}
}

// kvPrecisionLossless reports whether a KV-cache precision round-trips raw KV
// bytes without loss. Float precisions copy exactly; quantized precisions
// dequantize-then-requantize, so an evicted-and-readmitted cold block silently
// degrades. Empty defaults to f16 (the config default), which is lossless.
func kvPrecisionLossless(precision string) bool {
	switch strings.ToLower(strings.TrimSpace(precision)) {
	case "", "f16", "fp16", "f32", "fp32":
		return true
	default:
		return false
	}
}

var newEmbedSession = func(modelPath, device string) (EmbedSessionBackend, error) {
	return ovsession.NewEmbed(modelPath, device)
}

var _ transport.Session = (*genaiSession)(nil)
var _ residency.Controller = (*genaiSession)(nil)
var _ residency.Executor = (*genaiSession)(nil)

func (s *genaiSession) Capabilities() residency.Capabilities {
	cold := s.coldMaxTokens > 0 && s.coldKVLossless && s.coldKVBackend() != nil
	// OpenVINO GenAI owns physical KV blocks. The adapter exports cold blocks
	// and imports them back into destination prefix-cache blocks; shifted admits
	// rotate RoPE-positioned keys in the native GenAI bridge.
	return residency.Capabilities{
		RemoveTail:                   cold,
		RemoveMiddle:                 cold,
		PositionShift:                cold,
		SparseAttention:              s.sparseAttention,
		SlidingWindowAttentionTokens: s.slidingWindowAttentionTokens,
		ColdStore:                    cold,
		// The adapter owns the full transport token tape, so re-prefilling any
		// range from tokens is always physically executable — capabilities
		// report ability per the transport contract. Which path an admit takes
		// (native KV import when the lossless cold path is active, recompute
		// otherwise) is the executor's priority decision, not a capability.
		RecomputeRange: true,
	}
}

func (s *genaiSession) residentTokens() int { return len(s.resident) }

func (s *genaiSession) available() int {
	if s.numCtx <= 0 {
		return 0
	}
	return s.numCtx - s.residentTokens()
}

func (s *genaiSession) closedErrLocked() error {
	if s.fatalErr != "" {
		return fmt.Errorf("%w: %s", transport.ErrSessionFatal, s.fatalErr)
	}
	return transport.ErrSessionClosed
}

func (s *genaiSession) markFatalLocked(err error) error {
	if err == nil {
		err = transport.ErrSessionFatal
	}
	if s.fatalErr == "" {
		s.fatalErr = err.Error()
	}
	s.closed = true
	s.plannerCtx = 0
	s.coldMaxTokens = 0
	s.clearColdStoreLocked()
	s.stable = ""
	s.suffix = ""
	s.prefixText = ""
	s.stableMsgs = nil
	s.tools = ""
	s.resident = nil
	s.prefixLen = 0
	s.residencyPlan = residency.Plan{}
	s.residencyErr = "session fatal: " + s.fatalErr
	if s.backend != nil {
		_ = s.backend.Close()
	}
	return fmt.Errorf("%w: %v", transport.ErrSessionFatal, err)
}

func (s *genaiSession) backendError(stage string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if openvinoPoisonedPipelineError(err) || errors.Is(err, transport.ErrSessionFatal) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return fmt.Errorf("%w: %s: %v", s.markFatalLocked(err), stage, err)
	}
	return err
}

func (s *genaiSession) backendErrorLocked(stage string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Every non-cancellation backend failure on the locked (prefill/restore) path
	// is fatal for this session: the logical tape and physical KV may be out of
	// sync. Poison locally rather than only reporting ErrSessionFatal upstream, so
	// no fatal error ever leaves the session without poisoning it — the same
	// invariant the llama adapter enforces via fatalizeLocked.
	return fmt.Errorf("%w: %s: %v", s.markFatalLocked(err), stage, err)
}

func openvinoPoisonedPipelineError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	needles := []string{
		"BlockManager leaked",
		"BlockAllocator leaked",
		"leaked sequence block tables",
		"Expected num free blocks",
		"m_ref_count > 0",
	}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (s *genaiSession) shouldPhysicalPrefillLocked() bool {
	return !s.deferPhysicalPrefill || s.coldEnabledLocked()
}

func (s *genaiSession) prefillResidentLocked(ctx context.Context, tokens []int, stage string) error {
	if len(tokens) == 0 {
		return nil
	}
	if !s.shouldPhysicalPrefillLocked() {
		s.deferredPrefillCalls++
		s.deferredPrefillTokens += len(tokens)
		return nil
	}
	s.physicalPrefillCalls++
	s.physicalPrefillTokens += len(tokens)
	if err := s.backend.PrefillTokens(ctx, tokens); err != nil {
		return s.backendErrorLocked(stage, err)
	}
	return nil
}

func (s *genaiSession) recordDecodePrompt(tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decodeCalls++
	if tokens > 0 {
		s.decodePromptTokenCount += tokens
	}
}

func (s *genaiSession) tokenize(ctx context.Context, text string, addSpecial bool) ([]int, error) {
	if text == "" {
		return nil, nil
	}
	return s.backend.Tokenize(ctx, text, addSpecial)
}

func (s *genaiSession) EnsurePrefix(ctx context.Context, prefix transport.PrefixInput) (transport.PrefixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transport.PrefixStatus{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return transport.PrefixStatus{}, err
	}
	// This session keys reuse on a token-only tape; it has no projector and no
	// way to evaluate image parts. Refuse instead of silently dropping them —
	// vision requests belong to a VLM model dir, which OpenSession routes to
	// the vision session.
	if len(prefix.Images) > 0 {
		return transport.PrefixStatus{}, fmt.Errorf("%w: text-only openvino session cannot accept image input; open a vision-capable (VLM) model", transport.ErrUnsupportedFeature)
	}

	digest := prefix.Manifest.Digest()
	stableHash := prefix.Manifest.StableByteHash
	oldResident := s.residentTokens()

	stableMsgs := stableMessagesFromManifest(prefix.Text, prefix.Manifest)
	promptText := prefix.Text
	if len(stableMsgs) > 0 {
		templated, err := s.backend.ApplyChatTemplateWithPrompt(stableMsgs, prefix.Tools, false)
		if err != nil {
			return transport.PrefixStatus{}, fmt.Errorf("openvino: apply stable chat template: %w", err)
		}
		promptText = templated
	}

	tokens, err := s.tokenize(ctx, promptText, prefix.Manifest.AddBOS)
	if err != nil {
		return transport.PrefixStatus{}, fmt.Errorf("openvino: tokenize stable prefix: %w", err)
	}
	if s.numCtx > 0 && len(tokens) > s.numCtx {
		return transport.PrefixStatus{}, transport.ErrContextOverflow
	}
	if err := s.prefillResidentLocked(ctx, tokens, "openvino prefill stable prefix"); err != nil {
		return transport.PrefixStatus{}, err
	}

	// Enrich before committing any logical state: a template/tokenizer failure
	// here must leave the session as it was, not with a new resident tape under
	// the old manifest. (The physical prefill above is harmless prefix-cache
	// warming.) Tools are passed explicitly because s.tools still holds the
	// previous prefix's value at this point.
	enriched, err := s.enrichStableManifest(ctx, prefix.Text, prefix.Manifest, tokens, stableMsgs, prefix.Tools)
	if err != nil {
		return transport.PrefixStatus{}, err
	}

	reuse := 0
	compatible := false
	if ok, _ := s.manifest.CompatibleRuntime(prefix.Manifest); ok {
		compatible = true
		reuse = sessionkit.CommonPrefixLen(s.resident, tokens)
	}
	if !compatible || reuse < len(s.resident) {
		s.clearColdStoreLocked()
	}

	// EnsurePrefix replaces the stable prefix and drops any prior suffix.
	s.stable = prefix.Text
	s.suffix = ""
	s.prefixText = promptText
	s.stableMsgs = stableMsgs
	s.tools = prefix.Tools
	s.resident = append(s.resident[:0], tokens...)
	s.prefixLen = len(tokens)
	s.manifest = enriched
	s.updateResidencyPlanLocked(false)

	return transport.PrefixStatus{
		ReusedTokens:    reuse,
		PrefilledTokens: len(tokens) - reuse,
		DroppedTokens:   oldResident - reuse,
		PrefixTokens:    s.prefixLen,
		ResidentTokens:  s.residentTokens(),
		AvailableTokens: s.available(),
		StableByteHash:  stableHash,
		StableTokenHash: s.manifest.StableTokenHash,
		ManifestDigest:  digest,
	}, nil
}

func (s *genaiSession) PrefillSuffix(ctx context.Context, suffix transport.SuffixInput) (transport.SuffixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transport.SuffixStatus{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return transport.SuffixStatus{}, err
	}
	// Token-only tape: image parts cannot be evaluated here; refuse rather
	// than generate over a silently image-less prompt (see EnsurePrefix).
	if len(suffix.Images) > 0 {
		return transport.SuffixStatus{}, fmt.Errorf("%w: text-only openvino session cannot accept image input; open a vision-capable (VLM) model", transport.ErrUnsupportedFeature)
	}
	if ok, reason := s.manifest.CompatibleRuntime(suffix.Manifest); !ok {
		return transport.SuffixStatus{}, contextasm.NewManifestMismatchError(reason)
	}
	if !s.manifest.IsZero() && !suffix.Manifest.IsZero() && s.manifest.StableByteHash != suffix.Manifest.StableByteHash {
		return transport.SuffixStatus{}, contextasm.NewManifestMismatchError("stable prefix changed between EnsurePrefix and PrefillSuffix")
	}

	suffixManifest := suffix.Manifest
	if suffixManifest.StableBytes == 0 {
		suffixManifest.StableBytes = len(s.stable)
	}
	if suffixManifest.TotalBytes == 0 {
		suffixManifest.TotalBytes = len(s.stable) + len(suffix.Text)
	}
	volatileMsgs := volatileMessagesFromManifest(suffix.Text, suffixManifest)

	addSpecial := s.prefixLen == 0 && suffix.Manifest.AddBOS
	var tokens []int
	if len(s.stableMsgs)+len(volatileMsgs) > 0 {
		all := append(append([]ovsession.ChatMessage{}, s.stableMsgs...), volatileMsgs...)
		// Thinking/effort controls ride the generation-prompt render as
		// chat-template extra context, mirroring the llama backend's
		// renderTemplateForDecode kwargs.
		rendered, err := s.backend.ApplyChatTemplateWithOptions(all, s.tools, true, suffix.EnableThinking, suffix.ReasoningEffort)
		if err != nil {
			return transport.SuffixStatus{}, fmt.Errorf("openvino: apply full chat template: %w", err)
		}
		fullToks, err := s.tokenize(ctx, rendered, suffix.Manifest.AddBOS)
		if err != nil {
			return transport.SuffixStatus{}, fmt.Errorf("openvino: tokenize prompt: %w", err)
		}
		// Reconcile at the token level against the resident stable region instead of
		// byte-matching a separately rendered stable prefix. Some model templates are
		// not prefix-stable (e.g. phi-4-mini appends an end-of-text marker to a
		// system-only render), which byte-matching fatally rejected. Match only the
		// stable region so accumulated volatile turns are preserved; drop just a
		// diverging stable tail, then append this turn's volatile. prefillResidentLocked
		// re-prefills the reconciled token list, so the physical KV self-corrects.
		stableEnd := s.prefixLen
		if stableEnd > len(s.resident) {
			stableEnd = len(s.resident)
		}
		stableShared := sessionkit.CommonPrefixLen(s.resident[:stableEnd], fullToks)
		if stableShared < s.prefixLen {
			s.clearColdStoreLocked()
			s.resident = s.resident[:stableShared]
			s.prefixLen = stableShared
			s.prefixText = ""
		}
		tokens = fullToks[stableShared:]
	} else {
		toks, err := s.tokenize(ctx, suffix.Text, addSpecial)
		if err != nil {
			return transport.SuffixStatus{}, fmt.Errorf("openvino: tokenize suffix: %w", err)
		}
		tokens = toks
	}
	// Residency driver: when the suffix would overflow the hot window and a cold
	// store is available, park evictable ranges to host KV to make room instead
	// of overflowing or letting native eviction drop them. Effective context is
	// then numCtx hot plus the recoverable cold budget.
	if s.numCtx > 0 && s.coldEnabledLocked() && s.residentTokens()+len(tokens) > s.numCtx {
		if err := s.driveEvictToFitLocked(ctx, len(tokens)); err != nil {
			return transport.SuffixStatus{}, err
		}
	}
	if s.numCtx > 0 && !s.evictionEnabled && s.residentTokens()+len(tokens) > s.numCtx {
		return transport.SuffixStatus{}, transport.ErrContextOverflow
	}
	resident := append(append([]int(nil), s.resident...), tokens...)
	if err := s.prefillResidentLocked(ctx, resident, "openvino prefill suffix"); err != nil {
		return transport.SuffixStatus{}, err
	}
	enriched, err := s.enrichVolatileManifest(ctx, suffix.Text, suffixManifest, tokens, addSpecial, volatileMsgs)
	if err != nil {
		return transport.SuffixStatus{}, err
	}
	s.suffix += suffix.Text
	s.resident = resident
	s.manifest = enriched
	s.updateResidencyPlanLocked(true)

	return transport.SuffixStatus{
		SuffixTokens:    len(tokens),
		PrefixTokens:    s.prefixLen,
		ResidentTokens:  s.residentTokens(),
		AvailableTokens: s.available(),
		ManifestDigest:  suffix.Manifest.Digest(),
	}, nil
}

func (s *genaiSession) Decode(ctx context.Context, cfg transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	s.mu.Lock()
	if s.closed {
		err := s.closedErrLocked()
		s.mu.Unlock()
		return nil, err
	}
	fullText := s.stable + s.suffix
	manifest := s.manifest
	backend := s.backend
	tools := s.tools
	tokens := append([]int(nil), s.resident...)
	resident := s.residentTokens()
	numCtx := s.numCtx
	eviction := s.evictionEnabled
	s.mu.Unlock()

	opts := decodeOptions(cfg)
	out := make(chan transport.StreamChunk, 16)
	// With native cache eviction the pipeline bounds physical KV by evicting, so a
	// resident count at/over numCtx is not an overflow — let generation continue
	// past the window (the OpenVINO parallel to the llama slide). Without eviction
	// numCtx is a hard ceiling.
	if numCtx > 0 && !eviction && resident >= numCtx {
		go func() {
			defer close(out)
			_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: transport.ErrContextOverflow})
		}()
		return out, nil
	}
	if numCtx > 0 && !eviction && resident+opts.MaxNewTokens > numCtx {
		opts.MaxNewTokens = numCtx - resident
	}

	if len(tokens) > 0 {
		s.recordDecodePrompt(len(tokens))
		return s.decodePromptTokens(ctx, backend, tokens, opts, cfg, out)
	}

	prompt := fullText
	if msgs := chatMessagesFromManifest(fullText, manifest); len(msgs) > 0 {
		templated, err := backend.ApplyChatTemplate(msgs, tools)
		if err != nil {
			return nil, fmt.Errorf("openvino: apply chat template for decode: %w", err)
		}
		if strings.TrimSpace(templated) == "" {
			return nil, fmt.Errorf("openvino: apply chat template for decode returned empty prompt")
		}
		prompt = templated
	}
	s.recordDecodePrompt(0)
	return s.decodePrompt(ctx, backend, prompt, opts, cfg, out)
}

// enrichStableManifest computes token ranges for the stable manifest segments.
// It is pure with respect to session state — tools is the incoming prefix's tool
// JSON, passed explicitly so this can run before the session commits anything.
func (s *genaiSession) enrichStableManifest(ctx context.Context, stableText string, manifest transport.ContextManifest, tokens []int, stableMsgs []ovsession.ChatMessage, tools string) (transport.ContextManifest, error) {
	if len(manifest.Segments) == 0 {
		if manifest.StableBytes == 0 {
			manifest.StableBytes = len(stableText)
		}
		manifest.StableTokenHash = contextasm.HashTokenIDs(tokens)
		return manifest, nil
	}
	if len(stableMsgs) > 0 {
		return s.enrichStableChatManifest(ctx, stableText, manifest, tokens, stableMsgs, tools)
	}
	if manifest.StableBytes == 0 {
		manifest.StableBytes = len(stableText)
	}
	enriched, err := manifest.WithStableTokenization(stableText, tokens, func(text string, addSpecial bool) ([]int, error) {
		return s.tokenize(ctx, text, addSpecial)
	}, manifest.AddBOS)
	if err != nil {
		return transport.ContextManifest{}, err
	}
	return enriched, nil
}

func (s *genaiSession) enrichStableChatManifest(ctx context.Context, stableText string, manifest transport.ContextManifest, tokens []int, stableMsgs []ovsession.ChatMessage, tools string) (transport.ContextManifest, error) {
	out := manifest
	out.Segments = append([]contextasm.ManifestSegment(nil), manifest.Segments...)
	out.StableTokenHash = contextasm.HashTokenIDs(tokens)
	if out.StableBytes == 0 {
		out.StableBytes = len(stableText)
	}
	if out.StableBytes != len(stableText) {
		return transport.ContextManifest{}, contextasm.NewManifestMismatchError("stable byte length changed before tokenization")
	}
	prevEnd, msgIdx := 0, 0
	for i := range out.Segments {
		seg := &out.Segments[i]
		if !seg.Stable {
			continue
		}
		if sessionkit.ChatRole(seg.Kind) == "" {
			seg.TokenStart, seg.TokenEnd = prevEnd, prevEnd
			seg.TokenHash = contextasm.HashTokenIDs(tokens[prevEnd:prevEnd])
			continue
		}
		msgIdx++
		end := len(tokens)
		if msgIdx < len(stableMsgs) {
			rendered, err := s.backend.ApplyChatTemplateWithPrompt(stableMsgs[:msgIdx], tools, false)
			if err != nil {
				return transport.ContextManifest{}, fmt.Errorf("openvino: stable segment template: %w", err)
			}
			cum, err := s.tokenize(ctx, rendered, out.AddBOS)
			if err != nil {
				return transport.ContextManifest{}, fmt.Errorf("openvino: stable segment tokenize: %w", err)
			}
			end = len(cum)
		}
		if end < prevEnd || end > len(tokens) {
			return transport.ContextManifest{}, contextasm.NewManifestMismatchError("stable segment token boundary out of range")
		}
		seg.TokenStart, seg.TokenEnd = prevEnd, end
		seg.TokenHash = contextasm.HashTokenIDs(tokens[prevEnd:end])
		prevEnd = end
	}
	return out, nil
}

func (s *genaiSession) enrichVolatileManifest(ctx context.Context, suffixText string, manifest transport.ContextManifest, tokens []int, suffixAddSpecial bool, volatileMsgs []ovsession.ChatMessage) (transport.ContextManifest, error) {
	if len(manifest.Segments) == 0 {
		if manifest.StableBytes == 0 {
			manifest.StableBytes = len(s.stable)
		}
		if manifest.TotalBytes == 0 {
			manifest.TotalBytes = len(s.stable) + len(suffixText)
		}
		manifest.StableTokenHash = s.manifest.StableTokenHash
		manifest.VolatileTokenHash = contextasm.HashTokenIDs(tokens)
		return manifest, nil
	}
	if manifest.StableBytes == 0 {
		manifest.StableBytes = len(s.stable)
	}
	if manifest.TotalBytes == 0 {
		manifest.TotalBytes = len(s.stable) + len(suffixText)
	}
	if len(s.stableMsgs)+len(volatileMsgs) > 0 {
		return s.enrichVolatileChatManifest(ctx, suffixText, manifest, tokens, volatileMsgs)
	}
	enriched, err := manifest.WithVolatileTokenization(s.manifest, s.prefixLen, suffixText, tokens, func(text string, addSpecial bool) ([]int, error) {
		if suffixAddSpecial {
			addSpecial = true
		}
		return s.tokenize(ctx, text, addSpecial)
	})
	if err != nil {
		return transport.ContextManifest{}, err
	}
	return enriched, nil
}

func (s *genaiSession) enrichVolatileChatManifest(ctx context.Context, suffixText string, manifest transport.ContextManifest, tokens []int, volatileMsgs []ovsession.ChatMessage) (transport.ContextManifest, error) {
	if ok, reason := s.manifest.CompatibleRuntime(manifest); !ok {
		return transport.ContextManifest{}, contextasm.NewManifestMismatchError(reason)
	}
	if !s.manifest.IsZero() && !manifest.IsZero() && s.manifest.StableByteHash != manifest.StableByteHash {
		return transport.ContextManifest{}, contextasm.NewManifestMismatchError("stable prefix changed before suffix tokenization")
	}
	if manifest.StableBytes < 0 || manifest.TotalBytes < manifest.StableBytes || manifest.TotalBytes-manifest.StableBytes != len(suffixText) {
		return transport.ContextManifest{}, contextasm.NewManifestMismatchError("volatile byte length changed before tokenization")
	}

	out := manifest
	out.Segments = append([]contextasm.ManifestSegment(nil), manifest.Segments...)
	out.StableTokenHash = s.manifest.StableTokenHash
	out.VolatileTokenHash = contextasm.HashTokenIDs(tokens)
	mergeOpenVINOStableSegmentTokens(&out, s.manifest)
	for _, seg := range out.Segments {
		if seg.Stable && seg.TokenHash == "" {
			return transport.ContextManifest{}, contextasm.NewManifestMismatchError("stable segment token range missing from resident manifest")
		}
	}

	// Per-message volatile boundaries are advisory residency metadata over the
	// already-committed resident tape. They are derived by re-rendering message
	// prefixes and diffing token counts, which is only valid when the model's chat
	// template is token-prefix-additive per message. Real templates often are not
	// (tool/think blocks are placed by whole-conversation structure), so a partial
	// re-render tokenizes differently than the resident tape. On that inconsistency
	// fall back to coarse (unsplit) volatile residency instead of failing the turn;
	// genuine manifest inconsistencies above still error.
	type bound struct {
		start, end int
		hash       string
		set        bool
	}
	bounds := make([]bound, len(out.Segments))
	allMsgs := append(append([]ovsession.ChatMessage{}, s.stableMsgs...), volatileMsgs...)
	prevEnd, msgIdx := 0, len(s.stableMsgs)
	coarse := ""
	for i := range out.Segments {
		seg := &out.Segments[i]
		if seg.Stable {
			continue
		}
		if sessionkit.ChatRole(seg.Kind) == "" {
			bounds[i] = bound{s.prefixLen + prevEnd, s.prefixLen + len(tokens), contextasm.HashTokenIDs(tokens[prevEnd:]), true}
			prevEnd = len(tokens)
			continue
		}
		msgIdx++
		end := len(tokens)
		if msgIdx < len(allMsgs) {
			rendered, err := s.backend.ApplyChatTemplateWithPrompt(allMsgs[:msgIdx], s.tools, false)
			if err != nil {
				coarse = fmt.Sprintf("volatile segment template render failed: %v", err)
				break
			}
			if !strings.HasPrefix(rendered, s.prefixText) {
				coarse = "volatile segment boundaries unavailable (template not prefix-stable across message)"
				break
			}
			cum, err := s.tokenize(ctx, rendered[len(s.prefixText):], false)
			if err != nil {
				coarse = fmt.Sprintf("volatile segment tokenize failed: %v", err)
				break
			}
			end = len(cum)
		}
		if end < prevEnd || end > len(tokens) {
			coarse = fmt.Sprintf("volatile segment boundaries unavailable (template not token-prefix-additive): kind=%q", seg.Kind)
			break
		}
		bounds[i] = bound{s.prefixLen + prevEnd, s.prefixLen + end, contextasm.HashTokenIDs(tokens[prevEnd:end]), true}
		prevEnd = end
	}
	if coarse != "" {
		s.residencyErr = coarse
		return out, nil
	}
	for i := range out.Segments {
		if bounds[i].set {
			out.Segments[i].TokenStart = bounds[i].start
			out.Segments[i].TokenEnd = bounds[i].end
			out.Segments[i].TokenHash = bounds[i].hash
		}
	}
	return out, nil
}

func (s *genaiSession) updateResidencyPlanLocked(requireComplete bool) {
	s.residencyPlan = residency.Plan{}
	s.residencyErr = ""
	if s.numCtx <= 0 || len(s.resident) == 0 {
		return
	}
	blocks, err := residency.BlocksFromManifest(s.manifest, residency.ManifestOptions{
		ResidentTokens:  len(s.resident),
		RequireComplete: requireComplete,
	})
	if err != nil {
		s.residencyErr = err.Error()
		if len(blocks) == 0 {
			return
		}
	}
	plan, planErr := residency.PlanHotSet(residency.PlanInput{
		Blocks:       blocks,
		BudgetTokens: s.numCtx,
		StreamPolicy: s.streamPolicyLocked(),
		Capabilities: s.Capabilities(),
	})
	if planErr != nil {
		s.residencyErr = planErr.Error()
		return
	}
	if err != nil {
		plan.Diagnostics = append(plan.Diagnostics, err.Error())
	}
	s.residencyPlan = plan
}

// planForBudgetLocked computes a residency plan for the current resident tape
// under an explicit hot-token budget, without mutating s.residencyPlan. The
// overflow driver uses it to decide which ranges to park so an incoming prefill
// fits the hot window.
func (s *genaiSession) planForBudgetLocked(budget int) (residency.Plan, error) {
	if budget < 0 {
		budget = 0
	}
	blocks, err := residency.BlocksFromManifest(s.manifest, residency.ManifestOptions{
		ResidentTokens:  len(s.resident),
		RequireComplete: false,
	})
	if err != nil && len(blocks) == 0 {
		return residency.Plan{}, err
	}
	return residency.PlanHotSet(residency.PlanInput{
		Blocks:       blocks,
		BudgetTokens: budget,
		StreamPolicy: s.streamPolicyLocked(),
		Capabilities: s.Capabilities(),
	})
}

func (s *genaiSession) streamPolicyLocked() residency.StreamPolicy {
	caps := s.Capabilities()
	if !caps.RemoveMiddle || !caps.PositionShift {
		return residency.StreamPolicy{}
	}
	budget := residency.DeriveEvictionBudget(s.numCtx, s.slidingWindowAttentionTokens, openvinoEvictionBlock)
	return residency.StreamPolicy{
		Enabled:      budget.Valid(),
		SinkTokens:   budget.SinkTokens,
		RecentTokens: budget.RecentTokens,
	}
}

// driveEvictToFitLocked parks evictable ranges to the cold store so that the
// current resident plus `incoming` tokens fits numCtx. It executes the residency
// plan computed for the tightened budget (numCtx-incoming), evicting tail-first
// so earlier ranges keep stable indices. This is the inline residency driver:
// hot overflow parks recoverable context to host KV instead of erroring or
// letting native eviction drop it. Caller holds s.mu.
func (s *genaiSession) driveEvictToFitLocked(ctx context.Context, incoming int) error {
	plan, err := s.planForBudgetLocked(s.numCtx - incoming)
	if err != nil {
		return err
	}
	for _, r := range residency.EvictColdRanges(plan) {
		if s.residentTokens()+incoming <= s.numCtx {
			break
		}
		if err := s.evictRangeLocked(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *genaiSession) decodePrompt(ctx context.Context, backend genaiBackend, prompt string, opts ovsession.GenerateOptions, cfg transport.DecodeConfig, out chan transport.StreamChunk) (<-chan transport.StreamChunk, error) {
	if cfg.StructuredOutput.Protocol != "" || usesCompleteParser(opts.ParserProtocols) {
		go func() {
			defer close(out)
			res, err := backend.Generate(ctx, prompt, opts)
			if err != nil {
				_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: s.backendError("openvino generate", err)})
				return
			}
			chunk, err := chunkFromGenAIResult(res, cfg.StructuredOutput)
			if err != nil {
				_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: err})
				return
			}
			if !sessionkit.Send(ctx, out, chunk) {
				sessionkit.TrySend(out, transport.StreamChunk{Error: ctx.Err()})
			}
		}()
		return out, nil
	}

	src, err := backend.Stream(ctx, prompt, opts)
	if err != nil {
		return nil, s.backendError("openvino stream", err)
	}
	go func() {
		defer close(out)
		for chunk := range src {
			if chunk.Error != nil {
				chunk.Error = s.backendError("openvino stream", chunk.Error)
			}
			select {
			case out <- transport.StreamChunk{Text: chunk.Text, Thinking: chunk.Thinking, Error: chunk.Error}:
			case <-ctx.Done():
				sessionkit.TrySend(out, transport.StreamChunk{Error: ctx.Err()})
				return
			}
		}
	}()
	return out, nil
}

func (s *genaiSession) decodePromptTokens(ctx context.Context, backend genaiBackend, tokens []int, opts ovsession.GenerateOptions, cfg transport.DecodeConfig, out chan transport.StreamChunk) (<-chan transport.StreamChunk, error) {
	if cfg.StructuredOutput.Protocol != "" || usesCompleteParser(opts.ParserProtocols) {
		go func() {
			defer close(out)
			res, err := backend.GenerateTokens(ctx, tokens, opts)
			if err != nil {
				_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: s.backendError("openvino generate tokens", err)})
				return
			}
			chunk, err := chunkFromGenAIResult(res, cfg.StructuredOutput)
			if err != nil {
				_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: err})
				return
			}
			if !sessionkit.Send(ctx, out, chunk) {
				sessionkit.TrySend(out, transport.StreamChunk{Error: ctx.Err()})
			}
		}()
		return out, nil
	}

	src, err := backend.StreamTokens(ctx, tokens, opts)
	if err != nil {
		return nil, s.backendError("openvino stream tokens", err)
	}
	go func() {
		defer close(out)
		for chunk := range src {
			if chunk.Error != nil {
				chunk.Error = s.backendError("openvino stream tokens", chunk.Error)
			}
			select {
			case out <- transport.StreamChunk{Text: chunk.Text, Thinking: chunk.Thinking, Error: chunk.Error}:
			case <-ctx.Done():
				sessionkit.TrySend(out, transport.StreamChunk{Error: ctx.Err()})
				return
			}
		}
	}()
	return out, nil
}

func usesCompleteParser(protocols []string) bool {
	for _, protocol := range protocols {
		switch protocol {
		case "openvino:llama3_pythonic_tool_parser",
			"openvino:llama3_json_tool_parser",
			"openvino:reasoning_parser",
			"openvino:deepseek_r1_reasoning_parser",
			"openvino:phi4_reasoning_parser":
			return true
		}
	}
	return false
}

func chunkFromGenAIResult(res ovsession.GenAIResult, structured transport.StructuredOutputConfig) (transport.StreamChunk, error) {
	chunk := transport.StreamChunk{Text: res.Text}
	if structured.Protocol != "" && structured.Protocol != "openvino:json_schema_tool_calls" {
		return transport.StreamChunk{}, fmt.Errorf("openvino: unsupported structured output result protocol %q", structured.Protocol)
	}
	if strings.TrimSpace(res.ParsedJSON) == "" {
		if structured.Protocol == "" {
			return chunk, nil
		}
		return sessionkit.StructuredToolCallChunk(res.Text)
	}

	var parsed struct {
		Content         *string                     `json:"content"`
		Reasoning       *string                     `json:"reasoning_content"`
		ToolCalls       []sessionkit.ParsedToolCall `json:"tool_calls"`
		OpenAIToolCalls []sessionkit.ParsedToolCall `json:"toolCalls"`
	}
	if err := json.Unmarshal([]byte(res.ParsedJSON), &parsed); err != nil {
		return transport.StreamChunk{}, fmt.Errorf("openvino: parse GenAI parsed output: %w", err)
	}
	if parsed.Content != nil {
		chunk.Text = *parsed.Content
	}
	if parsed.Reasoning != nil {
		chunk.Thinking = *parsed.Reasoning
	}
	toolCalls := parsed.ToolCalls
	if len(toolCalls) == 0 {
		toolCalls = parsed.OpenAIToolCalls
	}
	if structured.Protocol == "openvino:json_schema_tool_calls" && parsed.Content == nil && len(toolCalls) == 0 {
		return transport.StreamChunk{}, fmt.Errorf("openvino: structured tool call parsed output contained neither content nor tool_calls")
	}
	calls, err := sessionkit.TransportToolCalls(toolCalls)
	if err != nil {
		return transport.StreamChunk{}, err
	}
	chunk.ToolCalls = calls
	return chunk, nil
}

func (s *genaiSession) ExplainContext() transport.ContextReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transport.ContextReport{
		ResidentTokens:          s.residentTokens(),
		PrefixTokens:            s.prefixLen,
		NumCtx:                  s.numCtx,
		HotContextTokens:        s.numCtx,
		PlannerEffectiveContext: s.plannerCtx,
		AvailableTokens:         s.available(),
		StableByteHash:          s.manifest.StableByteHash,
		StableTokenHash:         s.manifest.StableTokenHash,
		ManifestDigest:          s.manifest.Digest(),
		Manifest:                s.manifest,
		Closed:                  s.closed,
		FatalError:              s.fatalErr,
		PhysicalPrefillCalls:    s.physicalPrefillCalls,
		PhysicalPrefillTokens:   s.physicalPrefillTokens,
		DeferredPrefillCalls:    s.deferredPrefillCalls,
		DeferredPrefillTokens:   s.deferredPrefillTokens,
		DecodeCalls:             s.decodeCalls,
		DecodePromptTokens:      s.decodePromptTokenCount,
		Residency:               sessionkit.ResidencyReport(s.residencyPlan, s.residencyErr, s.Capabilities()),
	}
}

func (s *genaiSession) Snapshot(ctx context.Context) (transport.SessionSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transport.SessionSnapshot{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return transport.SessionSnapshot{}, err
	}
	return transport.SessionSnapshot{
		ResidentTokens:   s.residentTokens(),
		PrefixTokens:     s.prefixLen,
		NumCtx:           s.numCtx,
		ResidentTokenIDs: append([]int(nil), s.resident...),
		ColdKVBlocks:     s.coldBlockSnapshotsLocked(),
		StableText:       s.stable,
		PrefixText:       s.prefixText,
		Tools:            s.tools,
		Manifest:         s.manifest,
	}, nil
}

func (s *genaiSession) Restore(ctx context.Context, snap transport.SessionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snap.NumCtx > 0 && s.numCtx > 0 && snap.NumCtx != s.numCtx {
		return contextasm.NewManifestMismatchError("snapshot context window changed")
	}
	if snap.ResidentTokens < 0 || snap.PrefixTokens < 0 || snap.PrefixTokens > snap.ResidentTokens {
		return transport.ErrContextOverflow
	}
	if s.numCtx > 0 && snap.ResidentTokens > s.numCtx {
		return transport.ErrContextOverflow
	}
	if !s.manifest.IsZero() && !snap.Manifest.IsZero() {
		if ok, reason := s.manifest.CompatibleRuntime(snap.Manifest); !ok {
			return contextasm.NewManifestMismatchError(reason)
		}
	}
	stableMsgs := stableMessagesFromManifest(snap.StableText, snap.Manifest)
	prefixText := snap.PrefixText
	if prefixText == "" {
		if len(stableMsgs) > 0 {
			rendered, err := s.backend.ApplyChatTemplateWithPrompt(stableMsgs, snap.Tools, false)
			if err != nil {
				return fmt.Errorf("%w: openvino restore template: %v", transport.ErrSessionFatal, err)
			}
			prefixText = rendered
		} else {
			prefixText = snap.StableText
		}
	}
	if snap.StableText != "" && !strings.HasPrefix(prefixText, snap.StableText) {
		if len(stableMsgs) == 0 {
			return contextasm.NewManifestMismatchError("snapshot prefix text does not contain stable text")
		}
	}
	resident := append([]int(nil), snap.ResidentTokenIDs...)
	if len(resident) == 0 && snap.ResidentTokens > 0 {
		var err error
		resident, err = s.tokenize(ctx, prefixText, snap.Manifest.AddBOS)
		if err != nil {
			return fmt.Errorf("openvino: tokenize snapshot: %w", err)
		}
		if len(resident) != snap.ResidentTokens {
			return contextasm.NewManifestMismatchError("snapshot resident token count changed under tokenizer")
		}
	}
	if len(resident) != snap.ResidentTokens {
		return contextasm.NewManifestMismatchError("snapshot resident token ids do not match resident token count")
	}
	if err := s.prefillResidentLocked(ctx, resident, "openvino restore prefill"); err != nil {
		return err
	}
	s.stable = snap.StableText
	s.suffix = ""
	s.prefixText = prefixText
	s.stableMsgs = stableMsgs
	s.prefixLen = snap.PrefixTokens
	s.resident = resident
	if err := s.restoreColdBlocksLocked(snap.ColdKVBlocks); err != nil {
		return err
	}
	s.tools = snap.Tools
	s.manifest = snap.Manifest
	if s.manifest.StableTokenHash == "" && s.prefixLen <= len(s.resident) {
		s.manifest.StableTokenHash = contextasm.HashTokenIDs(s.resident[:s.prefixLen])
	}
	s.updateResidencyPlanLocked(true)
	return nil
}

func (s *genaiSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.plannerCtx = 0
	s.coldMaxTokens = 0
	s.clearColdStoreLocked()
	s.stable = ""
	s.suffix = ""
	s.prefixText = ""
	s.stableMsgs = nil
	s.resident = nil
	s.prefixLen = 0
	s.residencyPlan = residency.Plan{}
	s.residencyErr = ""
	if s.backend != nil {
		return s.backend.Close()
	}
	return nil
}

func stableMessagesFromManifest(text string, m transport.ContextManifest) []ovsession.ChatMessage {
	var msgs []ovsession.ChatMessage
	for _, seg := range m.Segments {
		if !seg.Stable {
			continue
		}
		role := sessionkit.ChatRole(seg.Kind)
		if role == "" {
			continue
		}
		if seg.ByteStart < 0 || seg.ByteEnd > len(text) || seg.ByteStart > seg.ByteEnd {
			continue
		}
		msgs = append(msgs, ovsession.ChatMessage{
			Role:       role,
			Content:    text[seg.ByteStart:seg.ByteEnd],
			ToolCalls:  seg.ToolCallsJSON,
			ToolCallID: seg.ToolCallID,
		})
	}
	return msgs
}

func volatileMessagesFromManifest(text string, m transport.ContextManifest) []ovsession.ChatMessage {
	var msgs []ovsession.ChatMessage
	base := m.StableBytes
	for _, seg := range m.Segments {
		if seg.Stable {
			continue
		}
		role := sessionkit.ChatRole(seg.Kind)
		if role == "" {
			continue
		}
		lo, hi := seg.ByteStart-base, seg.ByteEnd-base
		if lo < 0 || hi > len(text) || lo > hi {
			continue
		}
		msgs = append(msgs, ovsession.ChatMessage{
			Role:       role,
			Content:    text[lo:hi],
			ToolCalls:  seg.ToolCallsJSON,
			ToolCallID: seg.ToolCallID,
		})
	}
	return msgs
}

// chatMessagesFromManifest reconstructs all role/content turns from the
// assembled manifest's segments. It remains as a no-resident fallback for
// unusual callers that decode without prefilled tokens.
func chatMessagesFromManifest(fullText string, m transport.ContextManifest) []ovsession.ChatMessage {
	suffixText := ""
	if m.StableBytes >= 0 && m.StableBytes <= len(fullText) {
		suffixText = fullText[m.StableBytes:]
	}
	return append(stableMessagesFromManifest(fullText, m), volatileMessagesFromManifest(suffixText, m)...)
}

func mergeOpenVINOStableSegmentTokens(out *transport.ContextManifest, stable transport.ContextManifest) {
	stableByIdentity := map[contextasm.ManifestSegment]contextasm.ManifestSegment{}
	for _, seg := range stable.Segments {
		if !seg.Stable {
			continue
		}
		key := seg
		key.TokenStart = 0
		key.TokenEnd = 0
		key.TokenHash = ""
		stableByIdentity[key] = seg
	}
	for i := range out.Segments {
		seg := &out.Segments[i]
		if !seg.Stable {
			continue
		}
		key := *seg
		key.TokenStart = 0
		key.TokenEnd = 0
		key.TokenHash = ""
		if filled, ok := stableByIdentity[key]; ok {
			seg.TokenStart = filled.TokenStart
			seg.TokenEnd = filled.TokenEnd
			seg.TokenHash = filled.TokenHash
		}
	}
}

// decodeOptions maps the backend-neutral decode config onto OpenVINO GenAI's
// generate options.
func decodeOptions(cfg transport.DecodeConfig) ovsession.GenerateOptions {
	opts := ovsession.GenerateOptions{MaxNewTokens: cfg.MaxTokens, ParserProtocols: cfg.ParserProtocols}
	if opts.MaxNewTokens <= 0 {
		opts.MaxNewTokens = 256
	}
	if cfg.TopK > 0 {
		opts.TopK = &cfg.TopK
	}
	if cfg.Seed != nil && *cfg.Seed >= 0 {
		opts.Seed = cfg.Seed
	}
	if cfg.StructuredOutput.Protocol != "" {
		opts.StructuredOutput = ovsession.StructuredOutput{
			Protocol: openvinoStructuredProtocol(cfg.StructuredOutput.Protocol),
			Payload:  cfg.StructuredOutput.Payload,
		}
	}
	if cfg.Temperature != nil {
		v := *cfg.Temperature
		opts.Temperature = &v
	}
	if cfg.TopP != nil {
		v := *cfg.TopP
		opts.TopP = &v
	}
	return opts
}

func openvinoStructuredProtocol(protocol string) string {
	switch protocol {
	case "openvino:json_schema_tool_calls":
		return "openvino:triggered_tags"
	default:
		return protocol
	}
}
