package openvino

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
	"github.com/contenox/contenox/internal/modeld/openvino/ovsession"
	"github.com/contenox/contenox/libcontextasm"
	"github.com/contenox/contenox/libtransport"
)

// vlmBackend is the subset of *ovsession.VLMSession the vision transport-session
// adapter depends on. Narrowing it to an interface keeps the adapter logic
// testable without compiling the CGO OpenVINO GenAI backend.
type vlmBackend interface {
	// ApplyChatTemplate renders role/content turns with the MODEL's own chat
	// template. Generation then runs through VLMPipeline with its pipeline
	// template overridden to the identity template, so the self-rendered
	// prompt passes through unchanged (see ovsession/vlm.cpp).
	ApplyChatTemplate(messages []ovsession.ChatMessage, addGenerationPrompt bool) (string, error)
	Tokenize(ctx context.Context, prompt string, addSpecial bool) ([]int, error)
	Stream(ctx context.Context, prompt string, images []ovsession.VLMImage, opts ovsession.GenerateOptions) (<-chan ovsession.StreamChunk, error)
	Close() error
}

// The native OpenVINO VLM session is the production backend; the assertion
// holds in every build because the no-CGO stub mirrors its method set.
var _ vlmBackend = (*ovsession.VLMSession)(nil)

// probeVLMImage is the native image-decode probe, a seam for unit tests.
var probeVLMImage = func(data []byte) error {
	_, _, err := ovsession.ProbeVLMImage(data)
	return err
}

// visionSession adapts OpenVINO GenAI's VLMPipeline to the runtime's
// transport.Session contract for exported OpenVINO VLM directories.
//
// It is deliberately NOT the token-tape session (genaiSession): VLMPipeline
// exposes only a public generate() surface — its impl is a private pimpl with
// no prefix-cache or KV hook, unlike ContinuousBatchingPipeline which the text
// cell reaches internals of. Consequences, documented as the v1 truth:
//
//   - No prefix-cache reuse and no cold-KV/effective-context offload: every
//     Decode re-prefills the full multimodal prompt inside the pipeline.
//     EnsurePrefix/PrefillSuffix are logical accumulation + validation only.
//   - No residency.Controller/Executor: the session does not implement KV
//     surgery, so the residency planner never targets it.
//   - No snapshot/restore: SessionSnapshot cannot carry image payloads, and
//     silently restoring a conversation without its images would violate
//     refuse-don't-spill, so both operations return ErrUnsupportedFeature.
//
// Token accounting is text tokens (via the VLM tokenizer over the rendered
// conversation) plus a per-image estimate (ModelInfo.VisionTokensPerImage);
// the pipeline remains the hard gate on its real sequence limit.
type visionSession struct {
	backend              vlmBackend
	numCtx               int
	visionTokensPerImage int

	mu       sync.Mutex
	closed   bool
	manifest transport.ContextManifest
	stable   string
	suffix   string
	images   []transport.ImagePart

	stableTokens int // text tokens in the stable render
	textTokens   int // text tokens in the full conversation render

	decodeCalls            int
	decodePromptTokenCount int
}

func newVisionSession(backend vlmBackend, numCtx, visionTokensPerImage int) *visionSession {
	return &visionSession{
		backend:              backend,
		numCtx:               numCtx,
		visionTokensPerImage: visionTokensPerImage,
	}
}

var _ transport.Session = (*visionSession)(nil)

func (s *visionSession) estimatedTokensLocked() int {
	return s.textTokens + len(s.images)*s.visionTokensPerImage
}

func (s *visionSession) availableLocked() int {
	if s.numCtx <= 0 {
		return 0
	}
	return s.numCtx - s.estimatedTokensLocked()
}

// renderText renders the conversation for token accounting: through the
// model's chat template when the manifest declares chat turns, raw otherwise.
func (s *visionSession) renderText(msgs []ovsession.ChatMessage, raw string) (string, error) {
	if len(msgs) == 0 {
		return raw, nil
	}
	return s.backend.ApplyChatTemplate(msgs, false)
}

func (s *visionSession) countTokens(ctx context.Context, text string, addSpecial bool) (int, error) {
	if text == "" {
		return 0, nil
	}
	tokens, err := s.backend.Tokenize(ctx, text, addSpecial)
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}

func (s *visionSession) EnsurePrefix(ctx context.Context, prefix transport.PrefixInput) (transport.PrefixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transport.PrefixStatus{}, transport.ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return transport.PrefixStatus{}, err
	}
	// PrefixInput.Images documents that stable prefixes carrying images are
	// rejected; images belong to the volatile suffix, where every turn resends
	// them alongside their markers.
	if len(prefix.Images) > 0 {
		return transport.PrefixStatus{}, fmt.Errorf("%w: openvino VLM session does not accept images in the stable prefix; attach them to the volatile suffix", transport.ErrUnsupportedFeature)
	}
	// Model-native tool calls need the tools JSON rendered through the chat
	// template; the VLM cell does not wire that in v1.
	if prefix.Tools != "" {
		return transport.PrefixStatus{}, fmt.Errorf("%w: openvino VLM session does not support model-native tool definitions in v1", transport.ErrUnsupportedFeature)
	}

	oldEstimate := s.estimatedTokensLocked()
	stableMsgs := stableMessagesFromManifest(prefix.Text, prefix.Manifest)
	rendered, err := s.renderText(stableMsgs, prefix.Text)
	if err != nil {
		return transport.PrefixStatus{}, fmt.Errorf("openvino VLM: apply stable chat template: %w", err)
	}
	stableTokens, err := s.countTokens(ctx, rendered, prefix.Manifest.AddBOS)
	if err != nil {
		return transport.PrefixStatus{}, fmt.Errorf("openvino VLM: tokenize stable prefix: %w", err)
	}
	if s.numCtx > 0 && stableTokens > s.numCtx {
		return transport.PrefixStatus{}, transport.ErrContextOverflow
	}

	manifest := prefix.Manifest
	if manifest.StableBytes == 0 {
		manifest.StableBytes = len(prefix.Text)
	}

	// EnsurePrefix replaces the stable prefix and drops any prior suffix —
	// including its images.
	s.stable = prefix.Text
	s.suffix = ""
	s.images = nil
	s.manifest = manifest
	s.stableTokens = stableTokens
	s.textTokens = stableTokens

	return transport.PrefixStatus{
		// No physical KV: nothing is ever reused, and the "prefill" here is
		// logical accounting; the pipeline re-prefills at Decode.
		ReusedTokens:    0,
		PrefilledTokens: stableTokens,
		DroppedTokens:   oldEstimate,
		PrefixTokens:    stableTokens,
		ResidentTokens:  s.estimatedTokensLocked(),
		AvailableTokens: s.availableLocked(),
		StableByteHash:  prefix.Manifest.StableByteHash,
		ManifestDigest:  prefix.Manifest.Digest(),
	}, nil
}

func (s *visionSession) PrefillSuffix(ctx context.Context, suffix transport.SuffixInput) (transport.SuffixStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return transport.SuffixStatus{}, transport.ErrSessionClosed
	}
	if err := ctx.Err(); err != nil {
		return transport.SuffixStatus{}, err
	}
	if ok, reason := s.manifest.CompatibleRuntime(suffix.Manifest); !ok {
		return transport.SuffixStatus{}, contextasm.NewManifestMismatchError(reason)
	}
	if !s.manifest.IsZero() && !suffix.Manifest.IsZero() && s.manifest.StableByteHash != suffix.Manifest.StableByteHash {
		return transport.SuffixStatus{}, contextasm.NewManifestMismatchError("stable prefix changed between EnsurePrefix and PrefillSuffix")
	}
	// The transport contract couples markers and images 1:1 in reading order.
	// A mismatch is ambiguous (which image should be dropped, or where should
	// an unreferenced image go?), so it is refused, mirroring VLMPipeline's own
	// tag-count assertion.
	markers := strings.Count(suffix.Text, transport.MediaMarker)
	if markers != len(suffix.Images) {
		return transport.SuffixStatus{}, fmt.Errorf("%w: suffix carries %d media markers but %d images; each %q marker must reference exactly one image in order",
			transport.ErrUnsupportedFeature, markers, len(suffix.Images), transport.MediaMarker)
	}
	// Reject undecodable attachments at prefill time, before any generation
	// work: stb_image sniffs the real format from the bytes.
	for i, img := range suffix.Images {
		if err := probeVLMImage(img.Data); err != nil {
			return transport.SuffixStatus{}, fmt.Errorf("openvino VLM: image %d: %w", i, err)
		}
	}

	// suffix.EnableThinking / suffix.ReasoningEffort are chat-template extra
	// context on the text path. The VLM cell renders templates without extra
	// context in v1, which for templates that do not consume those variables
	// (the VLM exports validated here) is identical behavior.
	suffixManifest := suffix.Manifest
	if suffixManifest.StableBytes == 0 {
		suffixManifest.StableBytes = len(s.stable)
	}
	if suffixManifest.TotalBytes == 0 {
		suffixManifest.TotalBytes = len(s.stable) + len(suffix.Text)
	}

	newSuffix := s.suffix + suffix.Text
	fullText := s.stable + newSuffix
	msgs := chatMessagesFromManifest(fullText, suffixManifest)
	rendered, err := s.renderText(msgs, fullText)
	if err != nil {
		return transport.SuffixStatus{}, fmt.Errorf("openvino VLM: apply full chat template: %w", err)
	}
	textTokens, err := s.countTokens(ctx, rendered, suffix.Manifest.AddBOS)
	if err != nil {
		return transport.SuffixStatus{}, fmt.Errorf("openvino VLM: tokenize prompt: %w", err)
	}

	images := len(s.images) + len(suffix.Images)
	estimate := textTokens + images*s.visionTokensPerImage
	// No native eviction and no cold store on the VLM path: numCtx is a hard
	// ceiling for the estimated sequence.
	if s.numCtx > 0 && estimate > s.numCtx {
		return transport.SuffixStatus{}, transport.ErrContextOverflow
	}

	suffixTokens := textTokens - s.textTokens
	if suffixTokens < 0 {
		suffixTokens = 0
	}
	s.suffix = newSuffix
	s.images = append(s.images, suffix.Images...)
	s.manifest = suffixManifest
	s.textTokens = textTokens

	return transport.SuffixStatus{
		SuffixTokens:    suffixTokens,
		PrefixTokens:    s.stableTokens,
		ResidentTokens:  s.estimatedTokensLocked(),
		AvailableTokens: s.availableLocked(),
		ManifestDigest:  suffix.Manifest.Digest(),
	}, nil
}

// translateMediaMarkers rewrites the runtime's model-agnostic MediaMarker
// occurrences into GenAI's universal <ov_genai_image_i> tags, numbering
// markers in reading order across the conversation so tag i references
// images[i]. VLMPipeline's own prompt normalization then converts the
// universal tags into the model's native vision tags.
func translateMediaMarkers(msgs []ovsession.ChatMessage, imageCount int) ([]ovsession.ChatMessage, error) {
	idx := 0
	out := make([]ovsession.ChatMessage, len(msgs))
	for i, m := range msgs {
		for strings.Contains(m.Content, transport.MediaMarker) {
			m.Content = strings.Replace(m.Content, transport.MediaMarker, fmt.Sprintf("<ov_genai_image_%d>", idx), 1)
			idx++
		}
		out[i] = m
	}
	if idx != imageCount {
		return nil, fmt.Errorf("%w: conversation references %d media markers but %d images are attached",
			transport.ErrUnsupportedFeature, idx, imageCount)
	}
	return out, nil
}

func translateMediaMarkersText(text string, imageCount int) (string, error) {
	msgs, err := translateMediaMarkers([]ovsession.ChatMessage{{Role: "user", Content: text}}, imageCount)
	if err != nil {
		return "", err
	}
	return msgs[0].Content, nil
}

func (s *visionSession) Decode(ctx context.Context, cfg transport.DecodeConfig) (<-chan transport.StreamChunk, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, transport.ErrSessionClosed
	}
	fullText := s.stable + s.suffix
	manifest := s.manifest
	backend := s.backend
	images := append([]transport.ImagePart(nil), s.images...)
	estimate := s.estimatedTokensLocked()
	textTokens := s.textTokens
	numCtx := s.numCtx
	s.mu.Unlock()

	// The VLM cell has no structured-output or parser bridge in v1; refuse
	// instead of returning unparsed text under a structured contract.
	if cfg.StructuredOutput.Protocol != "" {
		return nil, fmt.Errorf("%w: openvino VLM session does not support structured output in v1", transport.ErrUnsupportedFeature)
	}
	if len(cfg.ParserProtocols) > 0 {
		return nil, fmt.Errorf("%w: openvino VLM session does not support parser protocols in v1", transport.ErrUnsupportedFeature)
	}

	opts := decodeOptions(cfg)
	out := make(chan transport.StreamChunk, 16)
	if numCtx > 0 && estimate >= numCtx {
		go func() {
			defer close(out)
			_ = sessionkit.Send(ctx, out, transport.StreamChunk{Error: transport.ErrContextOverflow})
		}()
		return out, nil
	}
	if numCtx > 0 && estimate+opts.MaxNewTokens > numCtx {
		opts.MaxNewTokens = numCtx - estimate
	}

	// Render the conversation with the model's own template (generation cue
	// included), with markers already translated to universal image tags.
	var prompt string
	if msgs := chatMessagesFromManifest(fullText, manifest); len(msgs) > 0 {
		translated, err := translateMediaMarkers(msgs, len(images))
		if err != nil {
			return nil, err
		}
		rendered, err := backend.ApplyChatTemplate(translated, true)
		if err != nil {
			return nil, fmt.Errorf("openvino VLM: apply chat template for decode: %w", err)
		}
		if strings.TrimSpace(rendered) == "" {
			return nil, fmt.Errorf("openvino VLM: apply chat template for decode returned empty prompt")
		}
		prompt = rendered
	} else {
		translated, err := translateMediaMarkersText(fullText, len(images))
		if err != nil {
			return nil, err
		}
		prompt = translated
	}

	vlmImages := make([]ovsession.VLMImage, len(images))
	for i, img := range images {
		vlmImages[i] = ovsession.VLMImage{Data: img.Data, MimeType: img.MimeType}
	}

	s.mu.Lock()
	s.decodeCalls++
	s.decodePromptTokenCount += textTokens
	s.mu.Unlock()

	src, err := backend.Stream(ctx, prompt, vlmImages, opts)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(out)
		for chunk := range src {
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

func (s *visionSession) ExplainContext() transport.ContextReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transport.ContextReport{
		ResidentTokens:          s.estimatedTokensLocked(),
		PrefixTokens:            s.stableTokens,
		NumCtx:                  s.numCtx,
		HotContextTokens:        s.numCtx,
		PlannerEffectiveContext: s.numCtx,
		AvailableTokens:         s.availableLocked(),
		StableByteHash:          s.manifest.StableByteHash,
		ManifestDigest:          s.manifest.Digest(),
		Manifest:                s.manifest,
		Closed:                  s.closed,
		DecodeCalls:             s.decodeCalls,
		DecodePromptTokens:      s.decodePromptTokenCount,
	}
}

// Snapshot is refused: transport.SessionSnapshot has no image payload field,
// so a restored VLM conversation would silently lose its images — the exact
// spill refuse-don't-spill forbids. VLM sessions are re-opened and re-fed
// instead (cheap, since there is no warm KV to preserve anyway).
func (s *visionSession) Snapshot(ctx context.Context) (transport.SessionSnapshot, error) {
	return transport.SessionSnapshot{}, fmt.Errorf("%w: openvino VLM session does not support snapshot in v1 (image attachments cannot ride a session snapshot)", transport.ErrUnsupportedFeature)
}

// Restore is refused for the same reason as Snapshot.
func (s *visionSession) Restore(ctx context.Context, snap transport.SessionSnapshot) error {
	return fmt.Errorf("%w: openvino VLM session does not support restore in v1 (image attachments cannot ride a session snapshot)", transport.ErrUnsupportedFeature)
}

func (s *visionSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.stable = ""
	s.suffix = ""
	s.images = nil
	if s.backend != nil {
		return s.backend.Close()
	}
	return nil
}
