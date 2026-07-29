//go:build llamanode && llamacpp_direct

package llamasession

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

// Vision support: a session whose model resolved with a multimodal projector
// (mmproj.gguf next to model.gguf, see llama.MMProjPathFor) accepts image
// parts on the volatile suffix. The prompt text carries one media marker
// (transport.MediaMarker) per image; mtmd splits the templated prompt at the
// markers, encodes each image through the projector, and the embeddings are
// decoded into the same sequence the text occupies.
//
// Image cells on the resident tape: the tape (s.resident) is a []int where
// index==position, but image positions have no token IDs — they hold encoder
// embeddings. Each image cell stores a negative sentinel derived from the
// image's content hash, so the token-level diff machinery keeps working:
// identical bytes resent on a later turn produce the same sentinel run (the
// reuse path can match it), while a changed image diverges at its first cell.
// A sentinel can never be replayed through the token decoder, which is why
// image-bearing prefills refuse the paths that would need that (see below).

// errNoVisionSupport reports the honest capability gap for image input.
func (s *session) errNoVisionSupport() error {
	if s.mtmd == nil {
		return llama.NewUnsupportedFeatureError("image input requires a vision model (no multimodal projector resolved next to the model)")
	}
	if !s.mtmd.SupportsVision() {
		return llama.NewUnsupportedFeatureError("the resolved multimodal projector does not support image input")
	}
	if s.mtmd.UsesMRoPE() {
		return llama.NewUnsupportedFeatureError("image input on M-RoPE models is not supported yet (image positions diverge from token counts)")
	}
	return nil
}

// tapeSpan maps one mtmd chunk onto its half-open range of the full tape.
type tapeSpan struct {
	start, end int
	chunkIdx   int
	image      bool
}

// imageCellSentinel derives the negative tape cell value for one image chunk
// from its content ID (mtmd assigns an FNV hash of the encoded bytes). Two
// different images colliding onto one sentinel would let the diff falsely
// reuse image KV; with a 30-bit fold over a handful of session images the
// probability is negligible and accepted.
func imageCellSentinel(id string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return -2 - int(h.Sum64()&0x3fffffff)
}

// prefillSuffixImagesLocked is the image-bearing counterpart of the
// PrefillSuffix text path: render the conversation (marker text included),
// tokenize it through mtmd into text+image chunks, diff the resulting tape
// against the resident stable region, and evaluate the divergent tail chunk by
// chunk. Caller holds s.mu and has validated the manifest.
func (s *session) prefillSuffixImagesLocked(ctx context.Context, suffix llama.SuffixInput, volatileMsgs []chatTemplateMessage) (llama.SuffixStatus, error) {
	if err := s.errNoVisionSupport(); err != nil {
		return llama.SuffixStatus{}, err
	}

	var prompt string
	addSpecial := s.addBOS
	if len(s.stableMsgs)+len(volatileMsgs) > 0 {
		all := append(append([]chatTemplateMessage{}, s.stableMsgs...), volatileMsgs...)
		rendered, err := s.renderTemplateForDecode(all, s.tools, suffix.EnableThinking, suffix.ReasoningEffort)
		if err != nil {
			return llama.SuffixStatus{}, fmt.Errorf("llamasession: apply chat template: %w", err)
		}
		prompt = rendered.Prompt
		s.chatSyntax = rendered.Syntax
	} else {
		prompt = suffix.Text
		addSpecial = s.prefixLen == 0 && s.addBOS
	}

	bitmaps := make([]*llamacppshim.MTMDBitmap, 0, len(suffix.Images))
	defer func() {
		for _, b := range bitmaps {
			b.Free()
		}
	}()
	for i, img := range suffix.Images {
		b, err := s.mtmd.BitmapFromImageBuf(img.Data)
		if err != nil {
			return llama.SuffixStatus{}, fmt.Errorf("llamasession: image %d (%s): %w", i, img.MimeType, err)
		}
		bitmaps = append(bitmaps, b)
	}
	chunks, err := s.mtmd.Tokenize(prompt, addSpecial, bitmaps)
	if err != nil {
		return llama.SuffixStatus{}, fmt.Errorf("llamasession: tokenize prompt with images: %w", err)
	}
	defer chunks.Free()

	tape, spans, err := s.chunkTape(chunks)
	if err != nil {
		return llama.SuffixStatus{}, err
	}

	// Reconcile against the resident stable region exactly like the text path
	// (see PrefillSuffix). Image cells only occur in the volatile region — the
	// stable prefix is token-only (EnsurePrefix rejects images) — so the shared
	// cut can never split an image chunk.
	stableEnd := s.prefixLen
	if stableEnd > len(s.resident) {
		stableEnd = len(s.resident)
	}
	stableShared := sessionkit.CommonPrefixLen(s.resident[:stableEnd], tape)
	if stableShared < s.prefixLen {
		if err := s.removeKV(stableShared, -1); err != nil {
			return llama.SuffixStatus{}, s.fatalizeLocked(err)
		}
		s.clearColdStoreLocked()
		s.resident = s.resident[:stableShared]
		s.prefixLen = stableShared
		s.prefixText = ""
	}
	stoks := len(tape) - stableShared

	// Refuse-don't-spill: an image-bearing prefill must fit the hot window
	// outright. The streaming/cold-parking overflow paths re-derive KV from
	// token IDs when they must recompute, which is impossible for image cells,
	// so instead of degrading we refuse with the honest budget.
	if len(s.resident)+stoks > s.numCtx {
		return llama.SuffixStatus{}, llama.NewContextOverflowError("suffix", len(s.resident), stoks, s.numCtx)
	}

	beforeLen := len(s.resident)
	if err := s.evalTapeSpansLocked(ctx, chunks, tape, spans, stableShared); err != nil {
		if rollbackErr := s.removeKV(beforeLen, -1); rollbackErr != nil {
			return llama.SuffixStatus{}, s.markFatalLocked(errors.Join(prefillFailureError("suffix", err), rollbackErr))
		}
		s.resident = s.resident[:beforeLen]
		if isContextErr(err) {
			return llama.SuffixStatus{}, err
		}
		return llama.SuffixStatus{}, s.markFatalLocked(prefillFailureError("suffix", err))
	}

	// Volatile segment enrichment re-renders message prefixes with plain
	// tokenization; against an image tape the boundaries cannot line up, so it
	// degrades to coarse residency with a diagnostic — by design (see
	// enrichVolatileSegments).
	_ = s.enrichVolatileSegments(s.prefixLen, volatileMsgs, tape[stableShared:])
	s.updateResidencyPlanLocked(true)
	return llama.SuffixStatus{
		SuffixTokens:    stoks,
		PrefixTokens:    s.prefixLen,
		ResidentTokens:  len(s.resident),
		AvailableTokens: s.numCtx - len(s.resident),
		ManifestDigest:  s.manifest.Digest(),
	}, nil
}

// chunkTape flattens mtmd chunks into the full logical tape (text token IDs,
// image sentinels) plus the span table mapping tape ranges back to chunks.
func (s *session) chunkTape(chunks *llamacppshim.MTMDChunks) ([]int, []tapeSpan, error) {
	var tape []int
	spans := make([]tapeSpan, 0, chunks.Len())
	for i := 0; i < chunks.Len(); i++ {
		start := len(tape)
		switch chunks.Type(i) {
		case llamacppshim.MTMDChunkText:
			tape = append(tape, chunks.TextTokens(i)...)
		case llamacppshim.MTMDChunkImage:
			n := chunks.NTokens(i)
			// Guarded by the M-RoPE refusal already; a mismatch here would
			// desynchronize tape indices from KV positions, so re-check.
			if chunks.NPos(i) != n {
				return nil, nil, llama.NewUnsupportedFeatureError(
					fmt.Sprintf("image chunk position count %d differs from token count %d", chunks.NPos(i), n))
			}
			id := chunks.ID(i)
			if id == "" {
				id = fmt.Sprintf("chunk-%d-%d", i, n)
			}
			cell := imageCellSentinel(id)
			for range n {
				tape = append(tape, cell)
			}
		default:
			return nil, nil, llama.NewUnsupportedFeatureError("audio input is not supported by the llama backend")
		}
		spans = append(spans, tapeSpan{start: start, end: len(tape), chunkIdx: i, image: chunks.Type(i) == llamacppshim.MTMDChunkImage})
	}
	return tape, spans, nil
}

// evalTapeSpansLocked evaluates tape[from:] into the KV at the current resident
// tail: text sub-ranges through the existing prefill path, image chunks through
// the mtmd helper (projector encode + embedding decode). Logits are requested
// on the very last evaluated cell, mirroring the text suffix path.
func (s *session) evalTapeSpansLocked(ctx context.Context, chunks *llamacppshim.MTMDChunks, tape []int, spans []tapeSpan, from int) error {
	for _, span := range spans {
		if span.end <= from {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		last := span.end == len(tape)
		if span.image {
			if span.start < from {
				// Unreachable while EnsurePrefix rejects images (the shared cut
				// stays inside the token-only stable region); refuse rather than
				// resume mid-image.
				return fmt.Errorf("llamasession: resident cut splits image chunk [%d,%d)", span.start, span.end)
			}
			n := span.end - span.start
			s.physicalPrefillCalls++
			s.physicalPrefillTokens += n
			newPast, err := s.mtmd.EvalChunk(s.lctx, chunks, span.chunkIdx, len(s.resident), 0, s.nBatch, last)
			if err != nil {
				return err
			}
			if got := newPast - len(s.resident); got != n {
				return fmt.Errorf("llamasession: image chunk advanced %d positions, want %d", got, n)
			}
			s.resident = append(s.resident, tape[span.start:span.end]...)
			continue
		}
		lo := span.start
		if lo < from {
			lo = from
		}
		if err := s.prefillAt(ctx, tape[lo:span.end], len(s.resident), last); err != nil {
			return err
		}
		s.resident = append(s.resident, tape[lo:span.end]...)
	}
	return nil
}

// guardTextSuffixMarkers rejects a text-only prefill whose rendered prompt
// still references media: on a vision session the marker is reserved for
// attached images, and silently tokenizing it as literal text would present
// the model a prompt that claims an image it never received (the multi-turn
// contract is that image bytes are resent with the history).
func (s *session) guardTextSuffixMarkers(rendered string) error {
	if s.mtmd == nil {
		return nil
	}
	if n := strings.Count(rendered, llamacppshim.MediaMarker()); n > 0 {
		return fmt.Errorf("llamasession: prompt references %d media marker(s) but no images are attached; resend the image parts with the conversation", n)
	}
	return nil
}
