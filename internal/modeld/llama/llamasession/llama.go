//go:build llamanode && llamacpp_direct

// Package llamasession is the llama.cpp adapter for llama.Session.
package llamasession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
	"github.com/contenox/contenox/internal/modeld/residency"
	"github.com/contenox/contenox/libcontextasm"
)

// Available reports whether the llama.cpp backend is compiled into this build.
const Available = true

// init registers this backend so the llama provider can create sessions
// without importing this CGo package (no import cycle). NewWithAdapters is the
// registered factory; New stays a no-adapter convenience that delegates to it.
func init() { llama.SetSessionFactory(NewWithAdapters) }

type session struct {
	mu                           sync.Mutex
	model                        *llamacppshim.Model
	lctx                         *llamacppshim.Context
	batch                        *llamacppshim.Batch
	mtmd                         *llamacppshim.MTMDContext // multimodal projector; nil for text-only models
	adapters                     []*llamacppshim.Adapter   // applied LoRA adapters, freed before the model on close
	numCtx                       int
	plannerCtx                   int
	coldMaxTokens                int
	coldTokens                   int
	coldClock                    int64
	coldBlocks                   map[string]*coldBlock
	coldRangeKey                 map[string]string
	sparseAttention              bool
	slidingWindowAttentionTokens int
	nBatch                       int
	addBOS                       bool
	resident                     []int  // token IDs currently in the KV (seq 0), in order
	prefixLen                    int    // how many of resident are the stable prefix
	stableText                   string // raw stable text from the runtime
	prefixText                   string // the templated stable text whose tokens are resident
	stableMsgs                   []chatTemplateMessage
	tools                        string // JSON tool definitions rendered into the prompt (model-native)
	manifest                     llama.ContextManifest
	chatSyntax                   llamacppshim.ChatSyntax
	reasoning                    string
	sessionLifecycle

	// Cumulative physical-work counters for ExplainContext observability,
	// matching the OpenVINO adapter's semantics: prefill counts native prefill
	// invocations/tokens; decodeCalls counts Decode requests. This backend has
	// no deferred-prefill mode and its decode carries no prompt tokens, so the
	// DeferredPrefill* and DecodePromptTokens contract fields stay a truthful
	// zero.
	physicalPrefillCalls  int
	physicalPrefillTokens int
	decodeCalls           int

	residencyPlan residency.Plan
}

var _ llama.Session = (*session)(nil)
var _ residency.Executor = (*session)(nil)

func (s *session) Capabilities() residency.Capabilities {
	return capabilitiesFor(s.sparseAttention, s.slidingWindowAttentionTokens, s.coldMaxTokens)
}

// EvictRange drops the resident token range [r.Start, r.End) from the KV cache
// and slides the surviving tail down by the removed width, so the remaining
// tokens keep contiguous RoPE positions (the StreamingLLM sliding-window move:
// remove the middle, shift the tail; llama.cpp re-applies RoPE to the shifted
// cells on the next decode). It updates the logical resident bookkeeping to
// match. The residency policy is responsible for never handing this primitive a
// protected range (attention sinks / task-pinned prefix); EvictRange trusts the
// range it is given.
func (s *session) EvictRange(ctx context.Context, r residency.Range) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fatalizeLocked(s.evictRangeLocked(r.Start, r.End))
}

// evictRangeLocked removes [a,b) from the KV, slides the tail, and updates
// resident bookkeeping. The caller holds s.mu and has checked s.closed.
func (s *session) evictRangeLocked(a, b int) error {
	n := len(s.resident)
	if a < 0 || b > n || a >= b {
		return fmt.Errorf("llamasession: evict range [%d,%d) outside resident [0,%d)", a, b, n)
	}
	block, err := s.exportColdBlockLocked(a, b)
	if err != nil {
		return err
	}
	if err := s.removeKV(a, b); err != nil {
		return err
	}
	if b < n {
		if err := s.addKVSeq(0, b, -1, -(b - a)); err != nil {
			return err
		}
	}
	s.resident = append(s.resident[:a], s.resident[b:]...)
	oldPrefix := s.prefixLen
	s.prefixLen = min(a, oldPrefix) + max(0, oldPrefix-b)
	if a < oldPrefix {
		s.prefixText = ""
	}
	if block != nil {
		s.storeColdBlockLocked(block)
	}
	return nil
}

func (s *session) slideForDecodeLocked() (bool, error) {
	recent := residency.DeriveEvictionBudget(s.numCtx, s.slidingWindowAttentionTokens, 1).RecentTokens
	a := s.prefixLen
	b := len(s.resident) - recent
	if b <= a {
		return false, nil
	}
	if err := s.evictRangeLocked(a, b); err != nil {
		return false, err
	}
	return true, nil
}

func (s *session) AdmitRange(ctx context.Context, r residency.Range) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fatalizeLocked(s.admitRangeLocked(ctx, r))
}

func New(modelPath string, cfg llama.Config) (llama.Session, error) {
	return NewWithAdapters(modelPath, cfg, nil)
}

func NewWithAdapters(modelPath string, cfg llama.Config, adapters []llama.AdapterSpec) (llama.Session, error) {
	model, err := llamacppshim.LoadModel(modelPath, modelConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("llamasession: load model %q: %w", modelPath, err)
	}

	numCtx := cfg.NumCtx
	if numCtx <= 0 {
		numCtx = 8192
	}
	numCtx -= numCtx % 2
	plannerCtx := cfg.PlannerEffectiveContext
	if plannerCtx <= 0 {
		plannerCtx = numCtx
	}
	if plannerCtx < numCtx {
		plannerCtx = numCtx
	}
	coldMaxTokens := max(plannerCtx-numCtx, 0)
	slidingWindow := model.SlidingWindowAttention()
	nBatch := cfg.NumBatch
	if nBatch <= 0 {
		nBatch = 512
	}
	addBOS := model.AddBOS() && !cfg.DisableBOS
	nThreads := cfg.NumThreads
	if nThreads <= 0 {
		nThreads = runtime.NumCPU()
	}

	lctx, err := llamacppshim.NewContext(model, llamacppshim.ContextConfig{
		NumCtx:      numCtx,
		NumBatch:    nBatch,
		NumSeqMax:   1 + min(coldMaxTokens, 1),
		NumThreads:  nThreads,
		FlashAttn:   flashAttnMode(cfg.FlashAttn),
		KVCacheType: cfg.KVCacheType,
		Embeddings:  false,
		OffloadKQV:  cfg.NumGpuLayers != 0,
		KVUnified:   coldMaxTokens > 0,
	})
	if err != nil {
		model.Close()
		return nil, fmt.Errorf("llamasession: new context: %w", err)
	}
	batch, err := llamacppshim.NewBatch(nBatch, 1, 0)
	if err != nil {
		lctx.Close()
		model.Close()
		return nil, fmt.Errorf("llamasession: new batch: %w", err)
	}

	loaded, err := applyAdapters(model, lctx, adapters)
	if err != nil {
		batch.Free()
		lctx.Close()
		model.Close()
		return nil, err
	}

	// A projector resolved next to the model loads eagerly: Describe already
	// budgeted its weights, and failing at open is the refuse-don't-spill
	// moment — not the first image request deep into a conversation.
	var mtmdCtx *llamacppshim.MTMDContext
	if mmprojPath := llama.MMProjPathFor(modelPath); mmprojPath != "" {
		mtmdCtx, err = llamacppshim.NewMTMDContext(model, mmprojPath, llamacppshim.MTMDConfig{
			UseGPU:     cfg.NumGpuLayers != 0,
			NumThreads: nThreads,
		})
		if err != nil {
			freeAdapters(loaded)
			batch.Free()
			lctx.Close()
			model.Close()
			return nil, fmt.Errorf("llamasession: load mmproj %q: %w", mmprojPath, err)
		}
	}

	return &session{
		model:                        model,
		lctx:                         lctx,
		batch:                        batch,
		mtmd:                         mtmdCtx,
		adapters:                     loaded,
		numCtx:                       numCtx,
		plannerCtx:                   plannerCtx,
		coldMaxTokens:                coldMaxTokens,
		sparseAttention:              slidingWindow > 0,
		slidingWindowAttentionTokens: slidingWindow,
		nBatch:                       nBatch,
		addBOS:                       addBOS,
		reasoning:                    cfg.ReasoningFormat,
	}, nil
}

func applyAdapters(model *llamacppshim.Model, lctx *llamacppshim.Context, adapters []llama.AdapterSpec) ([]*llamacppshim.Adapter, error) {
	if len(adapters) == 0 {
		return nil, nil
	}
	loaded := make([]*llamacppshim.Adapter, 0, len(adapters))
	for _, spec := range adapters {
		ad, err := model.LoadAdapter(spec.Path)
		if err != nil {
			freeAdapters(loaded)
			return nil, llama.NewUnsupportedFeatureError(fmt.Sprintf("lora adapter %q: %v", spec.Name, err))
		}
		scale := spec.Scale
		if scale == 0 {
			scale = 1.0
		}
		if err := lctx.SetAdapter(ad, scale); err != nil {
			ad.Free()
			freeAdapters(loaded)
			return nil, fmt.Errorf("llamasession: apply lora adapter %q: %w", spec.Name, err)
		}
		loaded = append(loaded, ad)
	}
	return loaded, nil
}

func freeAdapters(adapters []*llamacppshim.Adapter) {
	for _, ad := range adapters {
		ad.Free()
	}
}

func modelConfig(cfg llama.Config) llamacppshim.ModelConfig {
	return llamacppshim.ModelConfig{
		NumGPULayers: cfg.NumGpuLayers,
		TensorSplit:  cfg.TensorSplit,
		UseMmap:      true,
	}
}

func flashAttnMode(forceOn bool) llamacppshim.FlashAttnMode {
	if forceOn {
		return llamacppshim.FlashAttnOn
	}
	return llamacppshim.FlashAttnAuto
}

func (s *session) EnsurePrefix(ctx context.Context, prefix llama.PrefixInput) (llama.PrefixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return llama.PrefixStatus{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return llama.PrefixStatus{}, err
	}
	// Prefix reuse is keyed on a token-only tape (CommonPrefixLen over token
	// IDs, cold-store recompute, tail-logit replay); image cells would poison
	// every one of those paths, so the stable prefix stays text-only.
	if len(prefix.Images) > 0 {
		return llama.PrefixStatus{}, llama.NewUnsupportedFeatureError("images in the stable prefix are not supported; attach images to the volatile suffix")
	}

	stableMsgs := stableMessages(prefix.Text, prefix.Manifest)
	text := prefix.Text
	if len(stableMsgs) > 0 {
		templated, err := s.renderTemplate(stableMsgs, prefix.Tools, false)
		if err != nil {
			return llama.PrefixStatus{}, fmt.Errorf("llamasession: apply chat template: %w", err)
		}
		text = templated
	}

	toks, err := s.tokenize(text, s.addBOS)
	if err != nil {
		return llama.PrefixStatus{}, fmt.Errorf("llamasession: tokenize prefix: %w", err)
	}
	// Gate at the logical budget (hot window + cold budget), not the hot window:
	// a prefix larger than the hot window streams through it below, parking
	// completed ranges to the cold store. Without a cold store the logical
	// budget is the hot window and this stays the old hard gate.
	if len(toks) > s.logicalBudgetLocked() {
		return llama.PrefixStatus{}, llama.NewContextOverflowError("prefix", 0, len(toks), s.logicalBudgetLocked())
	}

	// Enrich before any KV mutation: a template/tokenizer failure here must leave
	// the session exactly as it was, not with a new resident tape under the old
	// manifest. Tools are passed explicitly because s.tools still holds the
	// previous prefix's value at this point.
	enriched, err := s.enrichStableSegments(prefix.Manifest, stableMsgs, toks, prefix.Tools)
	if err != nil {
		return llama.PrefixStatus{}, err
	}

	oldResident := len(s.resident)
	reuse := 0
	if ok, _ := s.manifest.CompatibleRuntime(prefix.Manifest); !ok {
		if err := s.removeKV(0, -1); err != nil {
			return llama.PrefixStatus{}, s.fatalizeLocked(err)
		}
		s.clearColdStoreLocked()
		s.resident = nil
		s.prefixLen = 0
		s.prefixText = ""
		s.manifest = llama.ContextManifest{}
	} else {
		reuse = sessionkit.CommonPrefixLen(s.resident, toks)
	}
	if reuse < len(s.resident) {
		if err := s.removeKV(reuse, -1); err != nil {
			return llama.PrefixStatus{}, s.fatalizeLocked(err)
		}
		s.clearColdStoreLocked()
		s.resident = s.resident[:reuse]
		if s.prefixLen > reuse {
			s.prefixLen = reuse
			s.prefixText = ""
		}
	}
	if len(toks) > s.numCtx {
		// Beyond-hot-window prefix: stream through the window, parking ranges to
		// cold as it fills. s.resident ends as the hot subset of toks; every
		// resident token belongs to the stable prefix. No rollback exists across
		// evictions — prefillStreamLocked closes the session on mid-stream error.
		if err := s.prefillStreamLocked(ctx, "prefix", toks[reuse:], false); err != nil {
			return llama.PrefixStatus{}, err
		}
		s.prefixLen = len(s.resident)
	} else {
		if err := s.prefillAt(ctx, toks[reuse:], reuse, false); err != nil {
			if rollbackErr := s.removeKV(reuse, -1); rollbackErr != nil {
				return llama.PrefixStatus{}, s.markFatalLocked(errors.Join(prefillFailureError("prefix", err), rollbackErr))
			}
			s.resident = s.resident[:reuse]
			if s.prefixLen > reuse {
				s.prefixLen = reuse
				s.prefixText = ""
			}
			if isContextErr(err) {
				return llama.PrefixStatus{}, err
			}
			return llama.PrefixStatus{}, s.markFatalLocked(prefillFailureError("prefix", err))
		}
		s.resident = toks
		s.prefixLen = len(toks)
	}
	s.stableText = prefix.Text
	s.prefixText = text
	s.stableMsgs = stableMsgs
	s.tools = prefix.Tools
	s.chatSyntax = llamacppshim.ChatSyntax{}
	s.manifest = enriched
	s.updateResidencyPlanLocked(false)

	return llama.PrefixStatus{
		ReusedTokens:    reuse,
		PrefilledTokens: len(toks) - reuse,
		DroppedTokens:   oldResident - reuse,
		PrefixTokens:    len(toks),
		ResidentTokens:  len(s.resident),
		AvailableTokens: s.numCtx - len(s.resident),
		StableByteHash:  prefix.Manifest.StableByteHash,
		StableTokenHash: contextasm.HashTokenIDs(toks),
		ManifestDigest:  prefix.Manifest.Digest(),
	}, nil
}

func (s *session) PrefillSuffix(ctx context.Context, suffix llama.SuffixInput) (llama.SuffixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return llama.SuffixStatus{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return llama.SuffixStatus{}, err
	}
	if ok, reason := s.manifest.CompatibleRuntime(suffix.Manifest); !ok {
		return llama.SuffixStatus{}, llama.NewManifestMismatchError(reason)
	}
	if !s.manifest.IsZero() && !suffix.Manifest.IsZero() && s.manifest.StableByteHash != suffix.Manifest.StableByteHash {
		return llama.SuffixStatus{}, llama.NewManifestMismatchError("stable prefix changed between EnsurePrefix and PrefillSuffix")
	}
	volatileMsgs := volatileMessages(suffix.Text, suffix.Manifest)
	if len(suffix.Images) > 0 {
		return s.prefillSuffixImagesLocked(ctx, suffix, volatileMsgs)
	}

	// Reconcile against the resident tape at the token level instead of byte-
	// matching a separately rendered stable prefix. Some model templates are not
	// prefix-stable: rendering the stable messages alone produces a different
	// string than the head of the full render — e.g. phi-4-mini appends an
	// end-of-text marker to a system-only render. Byte-matching those fatally
	// rejected the turn. Instead, render the whole conversation, tokenize it, and
	// keep the longest shared token prefix already resident; the leftover tokens
	// (including any stray marker from the isolated stable render) are diffed away
	// and re-prefilled. This is the template-agnostic definition of prefix caching
	// and never depends on the template being prefix-additive per message.
	var stoks []int
	if len(s.stableMsgs)+len(volatileMsgs) > 0 {
		all := append(append([]chatTemplateMessage{}, s.stableMsgs...), volatileMsgs...)
		rendered, err := s.renderTemplateForDecode(all, s.tools, suffix.EnableThinking, suffix.ReasoningEffort)
		if err != nil {
			return llama.SuffixStatus{}, fmt.Errorf("llamasession: apply chat template: %w", err)
		}
		if err := s.guardTextSuffixMarkers(rendered.Prompt); err != nil {
			return llama.SuffixStatus{}, err
		}
		fullToks, err := s.tokenize(rendered.Prompt, s.addBOS)
		if err != nil {
			return llama.SuffixStatus{}, fmt.Errorf("llamasession: tokenize prompt: %w", err)
		}
		s.chatSyntax = rendered.Syntax

		// This turn's new tokens are the full render minus the stable prefix already
		// resident. Match against the resident *stable region* (resident[:prefixLen]),
		// not the whole tape: volatile turns accumulated by earlier PrefillSuffix calls
		// must be preserved and appended to. Only a diverging stable *tail* is dropped
		// — e.g. the end-of-text marker phi-4-mini appends to a system-only render,
		// which never appears mid-conversation. This keeps prefixLen honest (residency
		// treats the true stable tokens as sinks) without a prefix-stable template
		// assumption.
		stableEnd := s.prefixLen
		if stableEnd > len(s.resident) {
			stableEnd = len(s.resident)
		}
		stableShared := sessionkit.CommonPrefixLen(s.resident[:stableEnd], fullToks)
		if stableShared < s.prefixLen {
			if err := s.removeKV(stableShared, -1); err != nil {
				return llama.SuffixStatus{}, s.fatalizeLocked(err)
			}
			s.clearColdStoreLocked()
			s.resident = s.resident[:stableShared]
			s.prefixLen = stableShared
			s.prefixText = ""
		}
		stoks = fullToks[stableShared:]
	} else {
		if err := s.guardTextSuffixMarkers(suffix.Text); err != nil {
			return llama.SuffixStatus{}, err
		}
		addSpecial := s.prefixLen == 0 && s.addBOS
		toks, err := s.tokenize(suffix.Text, addSpecial)
		if err != nil {
			return llama.SuffixStatus{}, fmt.Errorf("llamasession: tokenize suffix: %w", err)
		}
		stoks = toks
	}
	// Honest gate at the logical budget (hot window + cold budget when cold KV
	// is enabled): inputs within it are served by streaming below; beyond it is
	// a genuine overflow reported against the window that actually exists.
	if budget := s.logicalBudgetLocked(); len(s.resident)+len(stoks) > budget {
		if !s.coldEnabledLocked() && s.streamPolicyLocked().Enabled {
			// StreamingLLM-only session (no cold store): keep the historical
			// intentionally-lossy behavior — drop policy-selected middle ranges,
			// then require the suffix to fit the hot window.
			if err := s.driveEvictToFitLocked(len(stoks)); err != nil {
				return llama.SuffixStatus{}, s.fatalizeLocked(err)
			}
			if len(s.resident)+len(stoks) > s.numCtx {
				return llama.SuffixStatus{}, llama.NewContextOverflowError("suffix", len(s.resident), len(stoks), s.numCtx)
			}
		} else {
			return llama.SuffixStatus{}, llama.NewContextOverflowError("suffix", len(s.resident), len(stoks), budget)
		}
	}
	if s.coldEnabledLocked() && len(s.resident)+len(stoks) > s.numCtx {
		// Beyond-hot-window suffix: stream through the window, parking earlier
		// ranges to cold. No rollback exists across evictions —
		// prefillStreamLocked closes the session on mid-stream error.
		if err := s.prefillStreamLocked(ctx, "suffix", stoks, true); err != nil {
			return llama.SuffixStatus{}, err
		}
	} else {
		beforeLen := len(s.resident)
		if err := s.prefillAt(ctx, stoks, len(s.resident), true); err != nil {
			if rollbackErr := s.removeKV(beforeLen, -1); rollbackErr != nil {
				return llama.SuffixStatus{}, s.markFatalLocked(errors.Join(prefillFailureError("suffix", err), rollbackErr))
			}
			if isContextErr(err) {
				return llama.SuffixStatus{}, err
			}
			return llama.SuffixStatus{}, s.markFatalLocked(prefillFailureError("suffix", err))
		}
		s.resident = append(s.resident, stoks...)
	}
	// Volatile segment enrichment is advisory residency metadata over the already-
	// committed resident tape; it never fails the turn (it degrades to coarse
	// residency internally). The nil return is asserted for future-proofing.
	_ = s.enrichVolatileSegments(s.prefixLen, volatileMsgs, stoks)
	s.updateResidencyPlanLocked(true)
	return llama.SuffixStatus{
		SuffixTokens:    len(stoks),
		PrefixTokens:    s.prefixLen,
		ResidentTokens:  len(s.resident),
		AvailableTokens: s.numCtx - len(s.resident),
		ManifestDigest:  s.manifest.Digest(),
	}, nil
}

func (s *session) Decode(ctx context.Context, cfg llama.DecodeConfig) (<-chan llama.StreamChunk, error) {
	ch := make(chan llama.StreamChunk, 16)
	go func() {
		defer close(ch)
		s.mu.Lock()
		defer s.mu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				sessionkit.TrySend(ch, llama.StreamChunk{Error: s.markFatalLocked(fmt.Errorf("llamasession decode panicked: %v", r))})
			}
		}()
		if s.closed {
			if !sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.closedErrLocked()}) {
				return
			}
			return
		}
		if err := ctx.Err(); err != nil {
			sessionkit.TrySend(ch, llama.StreamChunk{Error: err})
			return
		}
		// Structured output constrains sampling with a GBNF grammar derived
		// from the request's JSON-schema payload (llama.cpp common's
		// json_schema_to_grammar, the same converter llama-server uses). The
		// tool-calls protocol additionally buffers the complete constrained
		// JSON and parses it into transport tool calls, mirroring the
		// OpenVINO adapter's semantics over its native structured engine.
		var grammarGBNF string
		structuredToolCalls := false
		switch cfg.StructuredOutput.Protocol {
		case "":
		case "json_schema", "llama:json_schema":
			g, err := llamacppshim.JSONSchemaToGrammar(cfg.StructuredOutput.Payload)
			if err != nil {
				sessionkit.TrySend(ch, llama.StreamChunk{Error: fmt.Errorf("llamasession: structured output schema: %w", err)})
				return
			}
			grammarGBNF = g
		case "llama:json_schema_tool_calls":
			g, err := llamacppshim.JSONSchemaToGrammar(cfg.StructuredOutput.Payload)
			if err != nil {
				sessionkit.TrySend(ch, llama.StreamChunk{Error: fmt.Errorf("llamasession: structured tool-call schema: %w", err)})
				return
			}
			grammarGBNF = g
			structuredToolCalls = true
		default:
			sessionkit.TrySend(ch, llama.StreamChunk{Error: llama.NewUnsupportedFeatureError(
				fmt.Sprintf("structured output protocol %q is not supported by the llama backend", cfg.StructuredOutput.Protocol))})
			return
		}
		s.decodeCalls++

		params := llamacppshim.SamplingParams{TopK: 40, TopP: 0.9, MinP: 0.05, Temperature: 0.8, GrammarGBNF: grammarGBNF}
		if cfg.TopK > 0 {
			params.TopK = cfg.TopK
		}
		if cfg.TopP != nil {
			params.TopP = float32(*cfg.TopP)
		}
		if cfg.Temperature != nil {
			params.Temperature = float32(*cfg.Temperature)
		}
		if cfg.Seed != nil && *cfg.Seed >= 0 {
			params.Seed = uint32(*cfg.Seed)
		}
		sampler, err := llamacppshim.NewSamplingContextForModel(s.model, params)
		if err != nil {
			if !sessionkit.Send(ctx, ch, llama.StreamChunk{Error: fmt.Errorf("llamasession: sampler: %w", err)}) {
				return
			}
			return
		}
		defer sampler.Free()

		maxTokens := cfg.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 256
		}
		if len(s.resident) >= s.numCtx {
			slid, err := s.slideForDecodeLocked()
			if err != nil {
				_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.markFatalLocked(fmt.Errorf("llamasession decode slide: %v", err))})
				return
			}
			if !slid {
				if !sessionkit.Send(ctx, ch, llama.StreamChunk{Error: llama.NewContextOverflowError("decode", len(s.resident), 1, s.numCtx)}) {
					return
				}
				return
			}
		}
		if err := s.refreshTailLogitsLocked(ctx); err != nil {
			_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: err})
			return
		}
		reasoningFormat := cfg.ReasoningFormat
		if reasoningFormat == "" {
			reasoningFormat = s.reasoning
		}
		// Grammar-constrained output is raw JSON, not chat syntax: the chat
		// output parser stays off. The tool-calls protocol buffers the whole
		// generation and parses it once at the end.
		var parser *chatOutputParser
		if grammarGBNF == "" {
			parser, err = newChatOutputParser(cfg.ParserProtocols, s.chatSyntax, reasoningFormat)
			if err != nil {
				if !sessionkit.Send(ctx, ch, llama.StreamChunk{Error: err}) {
					return
				}
				return
			}
		}
		var structuredBuf strings.Builder
		emitParsed := func(piece string, partial bool) bool {
			if structuredToolCalls {
				if partial {
					structuredBuf.WriteString(piece)
					return true
				}
				structuredBuf.WriteString(piece)
				chunk, err := sessionkit.StructuredToolCallChunk(structuredBuf.String())
				if err != nil {
					return sessionkit.Send(ctx, ch, llama.StreamChunk{Error: fmt.Errorf("llamasession: parse structured tool calls: %w", err)})
				}
				return sessionkit.Send(ctx, ch, chunk)
			}
			if parser == nil {
				if piece != "" {
					return sessionkit.Send(ctx, ch, llama.StreamChunk{Text: piece})
				}
				return true
			}
			text, thinking, toolCalls, err := parser.Push(piece, partial)
			if err != nil {
				return sessionkit.Send(ctx, ch, llama.StreamChunk{Error: err})
			}
			if text != "" || thinking != "" || len(toolCalls) > 0 {
				return sessionkit.Send(ctx, ch, llama.StreamChunk{Text: text, Thinking: thinking, ToolCalls: toolCalls})
			}
			return true
		}
		for n := 0; n < maxTokens; n++ {
			select {
			case <-ctx.Done():
				sessionkit.TrySend(ch, llama.StreamChunk{Error: ctx.Err()})
				return
			default:
			}
			// llama_sampler_sample accepts the selected token into the chain
			// itself; accepting again here would advance stateful samplers
			// (the grammar) twice and abort inside llama.cpp.
			id := sampler.Sample(s.lctx, -1)
			if id < 0 {
				_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.markFatalLocked(errors.New("llamasession decode sample returned no token"))})
				return
			}
			if s.model.TokenIsEOG(id) {
				emitParsed("", false)
				return
			}

			if len(s.resident) >= s.numCtx {
				slid, err := s.slideForDecodeLocked()
				if err != nil {
					_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.markFatalLocked(fmt.Errorf("llamasession decode slide: %v", err))})
					return
				}
				if !slid {
					emitParsed("", false)
					return
				}
			}

			s.batch.Clear()
			if err := s.batch.Add(id, nil, len(s.resident), true, 0); err != nil {
				_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.markFatalLocked(fmt.Errorf("llamasession decode batch: %v", err))})
				return
			}
			if res := s.lctx.Decode(s.batch); res.Status != llamacppshim.DecodeOK {
				_ = sessionkit.Send(ctx, ch, llama.StreamChunk{Error: s.markFatalLocked(fmt.Errorf("llamasession decode token: %v", res.Err))})
				return
			}
			s.resident = append(s.resident, id)
			if !emitParsed(s.model.TokenToPiece(id), true) {
				return
			}
		}
		emitParsed("", false)
	}()
	return ch, nil
}

func (s *session) ExplainContext() llama.ContextReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return llama.ContextReport{
		ResidentTokens:          len(s.resident),
		PrefixTokens:            s.prefixLen,
		NumCtx:                  s.numCtx,
		HotContextTokens:        s.numCtx,
		PlannerEffectiveContext: s.plannerCtx,
		AvailableTokens:         s.numCtx - len(s.resident),
		StableByteHash:          s.manifest.StableByteHash,
		StableTokenHash:         s.manifest.StableTokenHash,
		ManifestDigest:          s.manifest.Digest(),
		Manifest:                s.manifest,
		Closed:                  s.closed,
		FatalError:              s.fatalErr,
		PhysicalPrefillCalls:    s.physicalPrefillCalls,
		PhysicalPrefillTokens:   s.physicalPrefillTokens,
		DecodeCalls:             s.decodeCalls,
		Residency:               sessionkit.ResidencyReport(s.residencyPlan, s.residencyErr, s.Capabilities()),
	}
}

func (s *session) Snapshot(ctx context.Context) (llama.SessionSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return llama.SessionSnapshot{}, s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return llama.SessionSnapshot{}, err
	}
	state, err := s.lctx.StateGetData()
	if err != nil {
		// A failing native state read means the context itself is broken; the
		// resident KV can no longer be trusted, so poison instead of retrying.
		return llama.SessionSnapshot{}, s.markFatalLocked(fmt.Errorf("llamasession snapshot: %v", err))
	}
	return llama.SessionSnapshot{
		State:            state,
		ResidentTokens:   len(s.resident),
		PrefixTokens:     s.prefixLen,
		NumCtx:           s.numCtx,
		ResidentTokenIDs: append([]int(nil), s.resident...),
		ColdKVBlocks:     s.coldBlockSnapshotsLocked(),
		StableText:       s.stableText,
		PrefixText:       s.prefixText,
		Tools:            s.tools,
		Manifest:         s.manifest,
	}, nil
}

func (s *session) Restore(ctx context.Context, snap llama.SessionSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closedErrLocked()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snap.NumCtx > 0 && snap.NumCtx != s.numCtx {
		return llama.NewManifestMismatchError("snapshot context window changed")
	}
	if snap.ResidentTokens < 0 || snap.PrefixTokens < 0 || snap.PrefixTokens > snap.ResidentTokens {
		return fmt.Errorf("llamasession restore invalid snapshot: prefix_tokens=%d resident_tokens=%d", snap.PrefixTokens, snap.ResidentTokens)
	}
	if snap.ResidentTokens > s.numCtx {
		return llama.NewContextOverflowError("restore", 0, snap.ResidentTokens, s.numCtx)
	}
	if snap.ResidentTokens > 0 && len(snap.ResidentTokenIDs) != snap.ResidentTokens {
		return fmt.Errorf("llamasession restore invalid snapshot: resident_token_ids=%d resident_tokens=%d", len(snap.ResidentTokenIDs), snap.ResidentTokens)
	}
	if !s.manifest.IsZero() && !snap.Manifest.IsZero() {
		if ok, reason := s.manifest.CompatibleRuntime(snap.Manifest); !ok {
			return llama.NewManifestMismatchError(reason)
		}
	}
	if s.lctx == nil {
		return s.markFatalLocked(errors.New("llamasession restore context is nil"))
	}
	if snap.ResidentTokens > 0 && len(snap.State) == 0 {
		return fmt.Errorf("llamasession restore invalid snapshot: non-empty resident token set has empty state")
	}
	s.lctx.ClearMemory(true)
	if snap.ResidentTokens > 0 {
		if err := s.lctx.StateSetData(snap.State); err != nil {
			return s.markFatalLocked(fmt.Errorf("llamasession restore state: %v", err))
		}
	}
	s.resident = append([]int(nil), snap.ResidentTokenIDs...)
	s.prefixLen = snap.PrefixTokens
	if err := s.restoreColdBlocksLocked(snap.ColdKVBlocks); err != nil {
		return s.fatalizeLocked(err)
	}
	s.stableText = snap.StableText
	s.prefixText = snap.PrefixText
	s.tools = snap.Tools
	s.manifest = snap.Manifest
	s.stableMsgs = stableMessages(s.stableText, s.manifest)
	if s.prefixText == "" {
		if len(s.stableMsgs) > 0 {
			rendered, err := s.renderTemplate(s.stableMsgs, s.tools, false)
			if err != nil {
				return s.markFatalLocked(fmt.Errorf("llamasession restore template: %v", err))
			}
			s.prefixText = rendered
		} else {
			s.prefixText = s.stableText
		}
	}
	s.updateResidencyPlanLocked(true)
	return nil
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closeLocked()
	return nil
}

// closedErrLocked reports why this session is unusable: the recorded fatal
// cause when the backend poisoned it, plain ErrSessionClosed otherwise. The
// distinction is what lets the slot owner evict a poisoned session instead of
// treating it like an ordinary reopenable close.
func (s *session) closedErrLocked() error {
	return s.sessionLifecycle.closedErr()
}

// markFatalLocked poisons the session: records the first fatal cause, releases
// the native resources via closeLocked, and returns the cause wrapped in
// ErrSessionFatal. Every subsequent call observes the fatal state through
// closedErrLocked, so a poisoned llama.cpp context can never be reused.
func (s *session) markFatalLocked(err error) error {
	return s.sessionLifecycle.markFatal(err, s.closeLocked)
}

// fatalizeLocked routes an already-classified fatal error through the fatal
// state machine so no ErrSessionFatal ever leaves the session without
// poisoning it. Non-fatal errors pass through unchanged.
func (s *session) fatalizeLocked(err error) error {
	return s.sessionLifecycle.fatalize(err, s.closeLocked)
}

func (s *session) closeLocked() {
	if !s.sessionLifecycle.close() {
		return
	}
	if s.lctx != nil {
		s.lctx.ClearMemory(true)
		s.lctx.Close()
		s.lctx = nil
	}
	if s.batch != nil {
		s.batch.Free()
		s.batch = nil
	}
	if s.mtmd != nil {
		s.mtmd.Close()
		s.mtmd = nil
	}
	freeAdapters(s.adapters)
	s.adapters = nil
	if s.model != nil {
		s.model.Close()
		s.model = nil
	}
	s.resident = nil
	s.prefixLen = 0
	s.plannerCtx = 0
	s.coldMaxTokens = 0
	s.clearColdStoreLocked()
	s.stableText = ""
	s.prefixText = ""
	s.manifest = llama.ContextManifest{}
	s.residencyPlan = residency.Plan{}
	s.residencyErr = ""
}

func stableMessages(text string, m llama.ContextManifest) []chatTemplateMessage {
	var msgs []chatTemplateMessage
	for _, seg := range m.Segments {
		if !seg.Stable {
			continue
		}
		role := sessionkit.ChatRole(seg.Kind)
		if role == "" || seg.ByteStart < 0 || seg.ByteEnd > len(text) || seg.ByteStart > seg.ByteEnd {
			continue
		}
		msgs = append(msgs, chatTemplateMessage{
			Role:       role,
			Content:    text[seg.ByteStart:seg.ByteEnd],
			ToolCalls:  seg.ToolCallsJSON,
			ToolCallID: seg.ToolCallID,
		})
	}
	return msgs
}

func volatileMessages(text string, m llama.ContextManifest) []chatTemplateMessage {
	var msgs []chatTemplateMessage
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
		msgs = append(msgs, chatTemplateMessage{
			Role:       role,
			Content:    text[lo:hi],
			ToolCalls:  seg.ToolCallsJSON,
			ToolCallID: seg.ToolCallID,
		})
	}
	return msgs
}

// enrichStableSegments computes token ranges for the stable manifest segments.
// It is pure with respect to session state — tools is the incoming prefix's tool
// JSON, passed explicitly so this can run before the session commits anything.
func (s *session) enrichStableSegments(m llama.ContextManifest, stableMsgs []chatTemplateMessage, toks []int, tools string) (llama.ContextManifest, error) {
	m.Segments = append([]llama.ManifestSegment(nil), m.Segments...)
	m.StableTokenHash = contextasm.HashTokenIDs(toks)
	prevEnd, msgIdx := 0, 0
	for i := range m.Segments {
		seg := &m.Segments[i]
		if !seg.Stable {
			continue
		}
		if sessionkit.ChatRole(seg.Kind) == "" {
			seg.TokenStart, seg.TokenEnd = prevEnd, prevEnd
			seg.TokenHash = contextasm.HashTokenIDs(toks[prevEnd:prevEnd])
			continue
		}
		msgIdx++
		end := len(toks)
		if msgIdx < len(stableMsgs) {
			rendered, err := s.renderTemplate(stableMsgs[:msgIdx], tools, false)
			if err != nil {
				return llama.ContextManifest{}, fmt.Errorf("llamasession: stable segment template: %w", err)
			}
			cum, err := s.tokenize(rendered, s.addBOS)
			if err != nil {
				return llama.ContextManifest{}, fmt.Errorf("llamasession: stable segment tokenize: %w", err)
			}
			end = len(cum)
		}
		if end < prevEnd || end > len(toks) {
			return llama.ContextManifest{}, llama.NewManifestMismatchError("stable segment token boundary out of range")
		}
		seg.TokenStart, seg.TokenEnd = prevEnd, end
		seg.TokenHash = contextasm.HashTokenIDs(toks[prevEnd:end])
		prevEnd = end
	}
	return m, nil
}

// enrichVolatileSegments computes per-message token ranges for the volatile
// manifest segments. It is advisory: the ranges refine residency planning, but
// the resident token tape (s.resident) is already authoritative and correct.
//
// The per-message boundaries are derived by re-rendering prefixes of the message
// list and diffing token counts. That is only valid when the model's chat
// template is token-prefix-additive per message. Real templates often are not:
// tool definitions and thinking blocks are placed by whole-conversation
// structure, so a partial re-render tokenizes very differently than the resident
// tape (observed: a two-message re-render producing 2.7x the full conversation's
// tokens because the tool block only renders once a user turn is present). When
// that happens the fine-grained split is simply unavailable — the function falls
// back to coarse (unsplit) volatile residency and records a diagnostic, rather
// than failing the chat turn. It returns an error only for a genuine backend
// template/tokenize failure, which the caller also treats as non-fatal.
func (s *session) enrichVolatileSegments(prefixTokens int, volatileMsgs []chatTemplateMessage, stoks []int) error {
	segs := append([]llama.ManifestSegment(nil), s.manifest.Segments...)
	volatileHash := contextasm.HashTokenIDs(stoks)
	// Commit the copy and volatile hash up front; token ranges below are applied
	// only if the whole computation stays consistent, so a partial (and thus
	// possibly overlapping) result never lands in the manifest.
	commit := func(diag string) error {
		s.manifest.Segments = segs
		s.manifest.VolatileTokenHash = volatileHash
		if diag != "" {
			s.residencyErr = diag
		}
		return nil
	}

	type bound struct {
		start, end int
		hash       string
		set        bool
	}
	bounds := make([]bound, len(segs))
	allMsgs := append(append([]chatTemplateMessage{}, s.stableMsgs...), volatileMsgs...)
	prevEnd, msgIdx := 0, len(s.stableMsgs)
	for i := range segs {
		if segs[i].Stable {
			continue
		}
		if sessionkit.ChatRole(segs[i].Kind) == "" {
			bounds[i] = bound{prefixTokens + prevEnd, prefixTokens + len(stoks), contextasm.HashTokenIDs(stoks[prevEnd:]), true}
			prevEnd = len(stoks)
			continue
		}
		msgIdx++
		end := len(stoks)
		if msgIdx < len(allMsgs) {
			rendered, err := s.renderTemplate(allMsgs[:msgIdx], s.tools, false)
			if err != nil {
				return commit(fmt.Sprintf("volatile segment template render failed: %v", err))
			}
			cum, err := s.tokenize(rendered, s.addBOS)
			if err != nil {
				return commit(fmt.Sprintf("volatile segment tokenize failed: %v", err))
			}
			end = len(cum) - prefixTokens
		}
		if end < prevEnd || end > len(stoks) {
			// Template not token-prefix-additive for this turn: keep coarse residency.
			return commit(fmt.Sprintf("volatile segment boundaries unavailable (template not token-prefix-additive): kind=%q", segs[i].Kind))
		}
		bounds[i] = bound{prefixTokens + prevEnd, prefixTokens + end, contextasm.HashTokenIDs(stoks[prevEnd:end]), true}
		prevEnd = end
	}
	for i := range segs {
		if bounds[i].set {
			segs[i].TokenStart = bounds[i].start
			segs[i].TokenEnd = bounds[i].end
			segs[i].TokenHash = bounds[i].hash
		}
	}
	return commit("")
}

func (s *session) renderTemplate(msgs []chatTemplateMessage, tools string, addAssistant bool) (string, error) {
	result, err := s.renderTemplateWithOptions(msgs, tools, addAssistant, nil, "")
	if err != nil {
		return "", err
	}
	return result.Prompt, nil
}

func (s *session) renderTemplateForDecode(msgs []chatTemplateMessage, tools string, enableThinking *bool, reasoningEffort string) (llamacppshim.ChatTemplateResult, error) {
	return s.renderTemplateWithOptions(msgs, tools, true, enableThinking, reasoningEffort)
}

func (s *session) renderTemplateWithOptions(msgs []chatTemplateMessage, tools string, addAssistant bool, enableThinking *bool, reasoningEffort string) (llamacppshim.ChatTemplateResult, error) {
	msgsJSON, err := chatMessagesJSON(msgs)
	if err != nil {
		return llamacppshim.ChatTemplateResult{}, err
	}
	thinking := true
	if enableThinking != nil {
		thinking = *enableThinking
	}
	return s.model.ApplyChatTemplateCommonWithOptions(msgsJSON, tools, llamacppshim.ChatTemplateOptions{
		AddAssistant:    addAssistant,
		ReasoningFormat: s.reasoning,
		EnableThinking:  thinking,
		ReasoningEffort: reasoningEffort,
	})
}

func (s *session) tokenize(text string, addSpecial bool) ([]int, error) {
	return s.model.Tokenize(text, addSpecial, true)
}

const (
	llamaCommonChatReasoningParser = "llama:common_chat_reasoning_parser"
	llamaCommonChatToolParser      = "llama:common_chat_tool_parser"
)

type chatOutputParser struct {
	syntax          llamacppshim.ChatSyntax
	reasoningFormat string
	parseToolCalls  bool
	raw             strings.Builder
	content         string
	thinking        string
}

func newChatOutputParser(protocols []string, syntax llamacppshim.ChatSyntax, configuredReasoningFormat string) (*chatOutputParser, error) {
	var reasoningFormat string
	var parseToolCalls bool
	for _, protocol := range protocols {
		switch protocol {
		case "":
			continue
		case llamaCommonChatReasoningParser:
			if configuredReasoningFormat == "" {
				return nil, fmt.Errorf("%w: reasoning format is required for parser protocol %q", llama.ErrUnsupportedFeature, protocol)
			}
			reasoningFormat = configuredReasoningFormat
		case llamaCommonChatToolParser:
			parseToolCalls = true
		default:
			return nil, fmt.Errorf("%w: parser protocol %q", llama.ErrUnsupportedFeature, protocol)
		}
	}
	if reasoningFormat == "" && !parseToolCalls {
		return nil, nil
	}
	return &chatOutputParser{syntax: syntax, reasoningFormat: reasoningFormat, parseToolCalls: parseToolCalls}, nil
}

// parseChatResponse is the seam over llama.cpp's common chat parser. It is a
// package var so streaming-tolerance behavior can be unit-tested without a
// model (the CGo parser is grammar-dependent and, for lenient grammars, never
// exercises the partial-failure path this indirection lets tests drive).
var parseChatResponse = llamacppshim.ParseChatResponse

func (p *chatOutputParser) Push(piece string, partial bool) (textDelta, thinkingDelta string, toolCalls []llama.ToolCall, err error) {
	if p == nil {
		return piece, "", nil, nil
	}
	p.raw.WriteString(piece)
	raw := p.raw.String()
	parsed, err := parseChatResponse(raw, partial, p.syntax, p.reasoningFormat, p.parseToolCalls)
	if err != nil {
		// Mid-stream, the accumulated output legitimately may not parse yet.
		// llama.cpp's peg parser only recovers a partial parse when it made
		// some progress (common/chat.cpp: `if (is_partial && result.end > 0)`);
		// when a streamed fragment makes zero progress it throws
		// "does not match the expected <format> format" even with is_partial=1.
		// Do not fail the turn on that: keep accumulating and let a later parse
		// (or the authoritative final partial=false parse) emit the cumulative
		// delta. Only a failure on the final parse is fatal.
		if partial {
			return "", "", nil, nil
		}
		if p.parseToolCalls {
			textDelta, thinkingDelta, toolCalls, ok, fallbackErr := p.qwenToolCallFallback(raw)
			if fallbackErr != nil {
				return "", "", nil, fmt.Errorf("%w; qwen tool-call fallback failed: %v", p.parseError(err, partial, raw), fallbackErr)
			}
			if ok {
				return textDelta, thinkingDelta, toolCalls, nil
			}
		}
		return "", "", nil, p.parseError(err, partial, raw)
	}
	textDelta = stringDelta(p.content, parsed.Content)
	thinkingDelta = stringDelta(p.thinking, parsed.Thinking)
	p.content = parsed.Content
	p.thinking = parsed.Thinking
	if p.parseToolCalls && !partial && parsed.ToolCallsJSON != "" && parsed.ToolCallsJSON != "[]" {
		if err := json.Unmarshal([]byte(parsed.ToolCallsJSON), &toolCalls); err != nil {
			return "", "", nil, fmt.Errorf("llamasession: parse tool calls: %w", err)
		}
	}
	return textDelta, thinkingDelta, toolCalls, nil
}

func (p *chatOutputParser) qwenToolCallFallback(raw string) (textDelta, thinkingDelta string, toolCalls []llama.ToolCall, ok bool, err error) {
	if !strings.Contains(raw, "<tool_call>") {
		return "", "", nil, false, nil
	}
	chunk, err := sessionkit.StructuredToolCallChunk(raw)
	if err != nil {
		return "", "", nil, false, err
	}
	if len(chunk.ToolCalls) == 0 {
		return "", "", nil, false, nil
	}
	content := chunk.Text
	thinking := ""
	if strings.TrimSpace(content) != "" {
		parsed, err := parseChatResponse(content, false, p.syntax, p.reasoningFormat, false)
		if err == nil {
			content = parsed.Content
			thinking = parsed.Thinking
		}
	}
	textDelta = stringDelta(p.content, content)
	thinkingDelta = stringDelta(p.thinking, thinking)
	p.content = content
	p.thinking = thinking
	return textDelta, thinkingDelta, chunk.ToolCalls, true, nil
}

const maxChatParsePreviewRunes = 512

func (p *chatOutputParser) parseError(err error, partial bool, raw string) error {
	if p == nil {
		return err
	}
	return fmt.Errorf("%w (partial=%t, parse_tool_calls=%t, reasoning_format=%q, raw_len=%d, raw_preview=%q)",
		err, partial, p.parseToolCalls, p.reasoningFormat, len(raw), chatParsePreview(raw))
}

func chatParsePreview(raw string) string {
	raw = strings.ToValidUTF8(raw, "?")
	runes := []rune(raw)
	if len(runes) <= maxChatParsePreviewRunes {
		return raw
	}
	keep := maxChatParsePreviewRunes / 2
	return string(runes[:keep]) + "...<truncated>..." + string(runes[len(runes)-keep:])
}

func stringDelta(previous, current string) string {
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

func (s *session) removeKV(p0, p1 int) error {
	if s.lctx == nil {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession context is nil during kv remove", llama.ErrSessionFatal)
	}
	if !s.lctx.MemorySeqRemove(0, p0, p1) {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession kv remove failed seq=0 p0=%d p1=%d", llama.ErrSessionFatal, p0, p1)
	}
	return nil
}

func (s *session) copyKVSeq(srcSeqID, dstSeqID, p0, p1 int) error {
	if s.lctx == nil {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession context is nil during kv seq copy", llama.ErrSessionFatal)
	}
	if !s.lctx.MemorySeqCopy(srcSeqID, dstSeqID, p0, p1) {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession kv seq copy failed src=%d dst=%d p0=%d p1=%d", llama.ErrSessionFatal, srcSeqID, dstSeqID, p0, p1)
	}
	return nil
}

func (s *session) addKVSeq(seqID, p0, p1, delta int) error {
	if s.lctx == nil {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession context is nil during kv seq add", llama.ErrSessionFatal)
	}
	if !s.lctx.MemorySeqAdd(seqID, p0, p1, delta) {
		s.closeLocked()
		return fmt.Errorf("%w: llamasession kv seq add failed seq=%d p0=%d p1=%d delta=%d", llama.ErrSessionFatal, seqID, p0, p1, delta)
	}
	return nil
}

func (s *session) prefillAt(ctx context.Context, toks []int, startPos int, logitsOnLast bool) error {
	n := len(toks)
	if n > 0 {
		s.physicalPrefillCalls++
		s.physicalPrefillTokens += n
	}
	for i := 0; i < n; i += s.nBatch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		end := i + s.nBatch
		if end > n {
			end = n
		}
		s.batch.Clear()
		for j := i; j < end; j++ {
			if err := s.batch.Add(toks[j], nil, startPos+j, logitsOnLast && j == n-1, 0); err != nil {
				return fmt.Errorf("llamasession: prefill batch add: %w", err)
			}
		}
		if res := s.lctx.Decode(s.batch); res.Status != llamacppshim.DecodeOK {
			return fmt.Errorf("llamasession: prefill decode: %w", res.Err)
		}
	}
	return nil
}

// refreshTailLogitsLocked makes Decode robust after snapshot/restore and KV
// residency operations. llama.cpp state restoration can leave the output
// buffer empty even when KV is present; sampling then aborts inside C. Replaying
// the final resident token at its same position recreates the logits needed for
// the next sample without changing the logical token tape.
func (s *session) refreshTailLogitsLocked(ctx context.Context) error {
	if len(s.resident) == 0 {
		return llama.NewContextOverflowError("decode", 0, 1, s.numCtx)
	}
	lastPos := len(s.resident) - 1
	lastToken := s.resident[lastPos]
	// An image cell (negative sentinel, see vision.go) has no token to replay.
	// Chat templates always append a text generation prompt after media, so a
	// trailing image cell is a caller-contract breach, not a reachable state.
	if lastToken < 0 {
		return llama.NewUnsupportedFeatureError("decode requires the resident tape to end in a text token, not an image")
	}
	if err := s.removeKV(lastPos, -1); err != nil {
		return s.fatalizeLocked(err)
	}
	if err := s.prefillAt(ctx, []int{lastToken}, lastPos, true); err != nil {
		if isContextErr(err) {
			s.closeLocked()
			return err
		}
		return s.markFatalLocked(prefillFailureError("decode logits", err))
	}
	return nil
}

func (s *session) updateResidencyPlanLocked(requireComplete bool) {
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

// evictionDriverBlockSize splits large manifest segments into uniform blocks
// for the overflow/eviction driver. Without a split, a single big message (one
// segment = one block) can span the whole hot window and overlap both the sink
// head and the recent tail — flagged protected, leaving the driver nothing to
// evict even though most of the window is parkable middle. llama KV is
// token-granular, so the split costs nothing at the cache level; 256 balances
// cold-block granularity against per-block bookkeeping.
const evictionDriverBlockSize = 256

// planForBudgetLocked computes a residency plan for the current resident tape
// under an explicit hot-token budget, without mutating s.residencyPlan. The
// overflow driver uses it to decide which ranges to park so an incoming prefill
// fits the hot window. Mirrors the OpenVINO adapter.
func (s *session) planForBudgetLocked(budget int) (residency.Plan, error) {
	if budget < 0 {
		budget = 0
	}
	blocks, err := residency.BlocksFromManifest(s.manifest, residency.ManifestOptions{
		ResidentTokens:  len(s.resident),
		RequireComplete: false,
		BlockSize:       evictionDriverBlockSize,
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

func (s *session) streamPolicyLocked() residency.StreamPolicy {
	budget := residency.DeriveEvictionBudget(s.numCtx, s.slidingWindowAttentionTokens, 1)
	return residency.StreamPolicy{
		Enabled:      budget.Valid(),
		SinkTokens:   budget.SinkTokens,
		RecentTokens: budget.RecentTokens,
	}
}

// logicalBudgetLocked is the total token tape the session can hold: the hot
// window plus the cold-store budget when cold KV is enabled. Gating inputs at
// this budget (instead of the hot window) is what makes the planner-advertised
// context actually reachable, and it also guarantees streaming prefill never
// overflows the cold store into silent LRU drops. Caller holds s.mu.
func (s *session) logicalBudgetLocked() int {
	if s.coldEnabledLocked() && s.plannerCtx > s.numCtx {
		return s.plannerCtx
	}
	return s.numCtx
}

// prefillStreamLocked prefills an arbitrary-length token run by streaming it
// through the hot window: fill the free space, park policy-selected resident
// ranges to the cold store, repeat. Tokens keep index==position because
// evictRangeLocked compacts positions after each parking. logitsOnLast applies
// to the final token of the whole run. On any error after the tape has been
// mutated the session is closed: evictions cannot be rolled back, so a partial
// stream must never be presented as a usable state. Caller holds s.mu and has
// already gated len(toks) against logicalBudgetLocked.
func (s *session) prefillStreamLocked(ctx context.Context, stage string, toks []int, logitsOnLast bool) error {
	mutated := false
	fail := func(err error) error {
		if !mutated {
			return s.fatalizeLocked(err)
		}
		if isContextErr(err) {
			// Cancellation mid-stream leaves consistent but unusable state
			// (evictions cannot roll back): close, but the backend is healthy.
			s.closeLocked()
			return err
		}
		return s.markFatalLocked(prefillFailureError(stage, err))
	}
	off := 0
	for off < len(toks) {
		remaining := len(toks) - off
		space := s.numCtx - len(s.resident)
		if space <= 0 {
			freed, err := s.streamEvictLocked()
			if err != nil {
				mutated = mutated || freed > 0
				return fail(err)
			}
			if freed > 0 {
				mutated = true
			}
			space = s.numCtx - len(s.resident)
			if space <= 0 {
				// Nothing parkable (sink + recent fill the window): genuine overflow.
				return fail(llama.NewContextOverflowError(stage, len(s.resident), remaining, s.numCtx))
			}
		}
		n := min(space, remaining)
		last := logitsOnLast && off+n == len(toks)
		if err := s.prefillAt(ctx, toks[off:off+n], len(s.resident), last); err != nil {
			// prefillAt commits one nBatch batch at a time, so a mid-chunk failure
			// can leave KV cells past the resident tape. Roll the chunk back before
			// classifying, then mirror the non-streaming paths: cancellation keeps a
			// healthy backend (closed only when evictions already made the state
			// unusable), any real decode failure poisons the session.
			if rollbackErr := s.removeKV(len(s.resident), -1); rollbackErr != nil {
				return s.markFatalLocked(errors.Join(prefillFailureError(stage, err), rollbackErr))
			}
			if isContextErr(err) {
				if mutated {
					s.closeLocked()
				}
				return err
			}
			return s.markFatalLocked(prefillFailureError(stage, err))
		}
		mutated = true
		s.resident = append(s.resident, toks[off:off+n]...)
		off += n
	}
	return nil
}

// streamEvictLocked parks the middle of the resident tape — everything between
// the StreamingLLM sink head and the recent tail — to the cold store, in
// fixed-size blocks (tail-first, so earlier indices stay stable across the
// compaction each eviction performs). It deliberately does NOT consult the
// manifest-driven residency plan: mid-stream, the manifest only describes
// segments enriched on previous calls, so freshly streamed tokens have no
// blocks there and a plan-based driver would see nothing to evict. Returns the
// number of tokens parked. Caller holds s.mu.
func (s *session) streamEvictLocked() (int, error) {
	pol := s.streamPolicyLocked()
	if !pol.Enabled {
		return 0, nil
	}
	a := max(pol.SinkTokens, 0)
	b := len(s.resident) - max(pol.RecentTokens, 0)
	if b <= a {
		return 0, nil
	}
	freed := 0
	for end := b; end > a; {
		start := max(a, end-evictionDriverBlockSize)
		if err := s.evictRangeLocked(start, end); err != nil {
			return freed, err
		}
		freed += end - start
		end = start
	}
	return freed, nil
}

// driveEvictToFitLocked parks evictable ranges to the cold store so the current
// resident plus `incoming` tokens fits numCtx, executing the residency plan
// computed for the tightened budget. Ranges are evicted tail-first so earlier
// ranges keep stable indices. The inline residency driver: hot overflow parks
// recoverable context to host KV instead of overflowing. Caller holds s.mu.
func (s *session) driveEvictToFitLocked(incoming int) error {
	plan, err := s.planForBudgetLocked(s.numCtx - incoming)
	if err != nil {
		return err
	}
	for _, r := range residency.EvictColdRanges(plan) {
		if len(s.resident)+incoming <= s.numCtx {
			break
		}
		if err := s.evictRangeLocked(r.Start, r.End); err != nil {
			return err
		}
	}
	return nil
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func prefillFailureError(stage string, err error) error {
	if isContextErr(err) {
		return err
	}
	return fmt.Errorf("%w: llamasession prefill %s failed: %v", llama.ErrSessionFatal, stage, err)
}
