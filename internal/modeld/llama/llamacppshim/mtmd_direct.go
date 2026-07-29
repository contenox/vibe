//go:build llamacpp_direct

package llamacppshim

/*
#include <stdbool.h>
#include <stddef.h>
#include <stdlib.h>
#include "llama.h"
#include "mtmd.h"
#include "mtmd-helper.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"
)

// MediaMarker is the model-agnostic placeholder mtmd substitutes with media
// tokens during tokenization ("<__media__>"). Callers place one marker in the
// prompt text per attached image; the marker string itself never reaches the
// model.
func MediaMarker() string {
	return C.GoString(C.mtmd_default_marker())
}

// MMProjCaps reports the input modalities a multimodal projector (mmproj GGUF)
// declares, from a metadata-only read (no tensor load, no model required).
// Upstream reports an unreadable file as no capabilities rather than an error,
// so callers must stat the path separately if absence matters.
func MMProjCaps(path string) (vision, audio bool) {
	if path == "" {
		return false, false
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	caps := C.mtmd_get_cap_from_file(cpath)
	return bool(caps.inp_vision), bool(caps.inp_audio)
}

// MTMDConfig controls multimodal projector loading for the direct shim.
type MTMDConfig struct {
	UseGPU     bool
	NumThreads int
	// ImageMaxTokens caps the token count of one image for models with dynamic
	// resolution (0 = model default from projector metadata).
	ImageMaxTokens int
}

// MTMDContext wraps a direct mtmd_context: a multimodal projector loaded
// against an open text model. It owns the media encoder; the text model and
// llama context stay owned by their existing handles.
type MTMDContext struct {
	ptr *C.mtmd_context
}

// NewMTMDContext loads the projector at mmprojPath against model.
func NewMTMDContext(model *Model, mmprojPath string, cfg MTMDConfig) (*MTMDContext, error) {
	if model == nil || model.ptr == nil {
		return nil, errors.New("llamacppshim: model is closed")
	}
	if mmprojPath == "" {
		return nil, errors.New("llamacppshim: mmproj path is required")
	}
	initBackend()
	cpath := C.CString(mmprojPath)
	defer C.free(unsafe.Pointer(cpath))

	params := C.mtmd_context_params_default()
	params.use_gpu = C.bool(cfg.UseGPU)
	params.print_timings = C.bool(false)
	if cfg.NumThreads > 0 {
		params.n_threads = C.int(cfg.NumThreads)
	}
	if cfg.ImageMaxTokens > 0 {
		params.image_max_tokens = C.int(cfg.ImageMaxTokens)
	}
	ptr := C.mtmd_init_from_file(cpath, model.ptr, params)
	if ptr == nil {
		return nil, fmt.Errorf("llamacppshim: load mmproj %q", mmprojPath)
	}
	m := &MTMDContext{ptr: ptr}
	runtime.SetFinalizer(m, (*MTMDContext).Close)
	return m, nil
}

// Close releases the projector context.
func (m *MTMDContext) Close() {
	if m == nil || m.ptr == nil {
		return
	}
	C.mtmd_free(m.ptr)
	m.ptr = nil
	runtime.SetFinalizer(m, nil)
}

// SupportsVision reports whether the loaded projector accepts image input.
func (m *MTMDContext) SupportsVision() bool {
	if m == nil || m.ptr == nil {
		return false
	}
	return bool(C.mtmd_support_vision(m.ptr))
}

// UsesMRoPE reports whether decode uses M-RoPE for this model. Under M-RoPE an
// image chunk's temporal positions differ from its token count, which breaks
// any caller assuming index==position bookkeeping.
func (m *MTMDContext) UsesMRoPE() bool {
	if m == nil || m.ptr == nil {
		return false
	}
	return bool(C.mtmd_decode_use_mrope(m.ptr))
}

// MTMDBitmap wraps a decoded media bitmap (raw RGB pixels plus a content ID).
type MTMDBitmap struct {
	ptr *C.mtmd_bitmap
}

// BitmapFromImageBuf decodes an encoded image (PNG/JPEG/BMP/...) into a bitmap.
// Audio and video payloads are rejected: this shim surface is vision-only.
func (m *MTMDContext) BitmapFromImageBuf(data []byte) (*MTMDBitmap, error) {
	if m == nil || m.ptr == nil {
		return nil, errors.New("llamacppshim: mtmd context is closed")
	}
	if len(data) == 0 {
		return nil, errors.New("llamacppshim: empty image data")
	}
	wrapper := C.mtmd_helper_bitmap_init_from_buf(m.ptr, (*C.uchar)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.bool(false))
	if wrapper.video_ctx != nil {
		C.mtmd_helper_video_free(wrapper.video_ctx)
		if wrapper.bitmap != nil {
			C.mtmd_bitmap_free(wrapper.bitmap)
		}
		return nil, errors.New("llamacppshim: video input is not supported")
	}
	if wrapper.bitmap == nil {
		return nil, errors.New("llamacppshim: decode image (unsupported or corrupt format)")
	}
	if bool(C.mtmd_bitmap_is_audio(wrapper.bitmap)) {
		C.mtmd_bitmap_free(wrapper.bitmap)
		return nil, errors.New("llamacppshim: audio input is not supported")
	}
	b := &MTMDBitmap{ptr: wrapper.bitmap}
	runtime.SetFinalizer(b, (*MTMDBitmap).Free)
	return b, nil
}

// Free releases the bitmap. Safe to call more than once.
func (b *MTMDBitmap) Free() {
	if b == nil || b.ptr == nil {
		return
	}
	C.mtmd_bitmap_free(b.ptr)
	b.ptr = nil
	runtime.SetFinalizer(b, nil)
}

// ID returns the bitmap's content identity (an FNV hash of the encoded bytes,
// assigned by the decode helper). Stable across turns for identical bytes.
func (b *MTMDBitmap) ID() string {
	if b == nil || b.ptr == nil {
		return ""
	}
	return C.GoString(C.mtmd_bitmap_get_id(b.ptr))
}

// MTMDChunkType mirrors mtmd_input_chunk_type.
type MTMDChunkType int

const (
	MTMDChunkText MTMDChunkType = iota
	MTMDChunkImage
	MTMDChunkAudio
)

// MTMDChunks wraps the tokenization output: an ordered list of text and media
// chunks whose media entries are preprocessed but not yet encoded.
type MTMDChunks struct {
	ptr *C.mtmd_input_chunks
}

// Tokenize splits text at media markers (see MediaMarker) and pairs each marker
// with the corresponding bitmap, in order. The number of markers in text must
// equal len(bitmaps).
func (m *MTMDContext) Tokenize(text string, addSpecial bool, bitmaps []*MTMDBitmap) (*MTMDChunks, error) {
	if m == nil || m.ptr == nil {
		return nil, errors.New("llamacppshim: mtmd context is closed")
	}
	// Validate the pairing up front: upstream reports the two mismatch
	// directions under different (and overlapping) return codes, so the only
	// deterministic contract error comes from counting here.
	if want := strings.Count(text, MediaMarker()); want != len(bitmaps) {
		return nil, fmt.Errorf("llamacppshim: prompt has %d media marker(s) but %d attached image(s)", want, len(bitmaps))
	}
	ctext := C.CString(text)
	defer C.free(unsafe.Pointer(ctext))
	itext := C.mtmd_input_text{
		text:          ctext,
		add_special:   C.bool(addSpecial),
		parse_special: C.bool(true),
	}
	// The elements are C pointers, so passing the Go slice's backing array to C
	// is legal under the cgo pointer rules.
	ptrs := make([]*C.mtmd_bitmap, 0, len(bitmaps))
	for _, b := range bitmaps {
		if b == nil || b.ptr == nil {
			return nil, errors.New("llamacppshim: nil bitmap")
		}
		ptrs = append(ptrs, b.ptr)
	}
	var cptrs **C.mtmd_bitmap
	if len(ptrs) > 0 {
		cptrs = &ptrs[0]
	}
	chunks := C.mtmd_input_chunks_init()
	if chunks == nil {
		return nil, errors.New("llamacppshim: allocate input chunks")
	}
	rc := C.mtmd_tokenize(m.ptr, chunks, &itext, cptrs, C.size_t(len(ptrs)))
	// The wrapper objects must outlive the C call or a finalizer could free a
	// bitmap mid-tokenize; the ptrs slice only keeps the C pointers alive.
	runtime.KeepAlive(bitmaps)
	if rc != 0 {
		C.mtmd_input_chunks_free(chunks)
		switch rc {
		case 1:
			return nil, fmt.Errorf("llamacppshim: media marker count does not match %d attached image(s)", len(ptrs))
		case 2:
			return nil, errors.New("llamacppshim: image preprocessing failed")
		default:
			return nil, fmt.Errorf("llamacppshim: mtmd tokenize failed (rc=%d)", int(rc))
		}
	}
	c := &MTMDChunks{ptr: chunks}
	runtime.SetFinalizer(c, (*MTMDChunks).Free)
	return c, nil
}

// Free releases the chunk list (and every chunk in it).
func (c *MTMDChunks) Free() {
	if c == nil || c.ptr == nil {
		return
	}
	C.mtmd_input_chunks_free(c.ptr)
	c.ptr = nil
	runtime.SetFinalizer(c, nil)
}

// Len returns the number of chunks.
func (c *MTMDChunks) Len() int {
	if c == nil || c.ptr == nil {
		return 0
	}
	return int(C.mtmd_input_chunks_size(c.ptr))
}

func (c *MTMDChunks) chunk(i int) *C.mtmd_input_chunk {
	if c == nil || c.ptr == nil || i < 0 || i >= c.Len() {
		return nil
	}
	return C.mtmd_input_chunks_get(c.ptr, C.size_t(i))
}

// Type returns chunk i's kind.
func (c *MTMDChunks) Type(i int) MTMDChunkType {
	chunk := c.chunk(i)
	if chunk == nil {
		return MTMDChunkText
	}
	switch C.mtmd_input_chunk_get_type(chunk) {
	case C.MTMD_INPUT_CHUNK_TYPE_IMAGE:
		return MTMDChunkImage
	case C.MTMD_INPUT_CHUNK_TYPE_AUDIO:
		return MTMDChunkAudio
	default:
		return MTMDChunkText
	}
}

// TextTokens returns chunk i's token IDs (text chunks only; nil otherwise).
func (c *MTMDChunks) TextTokens(i int) []int {
	chunk := c.chunk(i)
	if chunk == nil || C.mtmd_input_chunk_get_type(chunk) != C.MTMD_INPUT_CHUNK_TYPE_TEXT {
		return nil
	}
	var n C.size_t
	toks := C.mtmd_input_chunk_get_tokens_text(chunk, &n)
	if toks == nil || n == 0 {
		return nil
	}
	out := make([]int, int(n))
	for j, t := range unsafe.Slice(toks, int(n)) {
		out[j] = int(t)
	}
	// c's finalizer would free the token storage read above.
	runtime.KeepAlive(c)
	return out
}

// NTokens returns chunk i's token count (sequence cells it occupies).
func (c *MTMDChunks) NTokens(i int) int {
	chunk := c.chunk(i)
	if chunk == nil {
		return 0
	}
	return int(C.mtmd_input_chunk_get_n_tokens(chunk))
}

// NPos returns chunk i's temporal position count. Equals NTokens except under
// M-RoPE (see MTMDContext.UsesMRoPE).
func (c *MTMDChunks) NPos(i int) int {
	chunk := c.chunk(i)
	if chunk == nil {
		return 0
	}
	return int(C.mtmd_input_chunk_get_n_pos(chunk))
}

// ID returns chunk i's media content ID ("" for text chunks).
func (c *MTMDChunks) ID(i int) string {
	chunk := c.chunk(i)
	if chunk == nil {
		return ""
	}
	id := C.mtmd_input_chunk_get_id(chunk)
	if id == nil {
		return ""
	}
	return C.GoString(id)
}

// TotalPos is the total temporal positions across all chunks (the n_past
// advance a full evaluation would produce).
func (c *MTMDChunks) TotalPos() int {
	if c == nil || c.ptr == nil {
		return 0
	}
	return int(C.mtmd_helper_get_n_pos(c.ptr))
}

// EvalChunk evaluates chunk i into lctx at position nPast on seqID: text chunks
// run through llama_decode, media chunks are encoded by the projector and their
// embeddings decoded (mtmd_helper_eval_chunk_single, which also manages the
// non-causal attention window some vision models need). Returns the new n_past.
func (m *MTMDContext) EvalChunk(lctx *Context, chunks *MTMDChunks, i int, nPast, seqID, nBatch int, logitsLast bool) (int, error) {
	if m == nil || m.ptr == nil {
		return 0, errors.New("llamacppshim: mtmd context is closed")
	}
	if lctx == nil || lctx.ptr == nil {
		return 0, errors.New("llamacppshim: context is closed")
	}
	chunk := chunks.chunk(i)
	if chunk == nil {
		return 0, fmt.Errorf("llamacppshim: chunk index %d out of range", i)
	}
	if nBatch <= 0 {
		nBatch = 512
	}
	// mtmd_helper_eval_chunk_single accumulates the text-chunk advance into
	// *new_n_past, so it must start at nPast (upstream callers alias it with
	// their running n_past).
	newPast := C.llama_pos(nPast)
	rc := C.mtmd_helper_eval_chunk_single(m.ptr, lctx.ptr, chunk, C.llama_pos(nPast), C.llama_seq_id(seqID), C.int32_t(nBatch), C.bool(logitsLast), &newPast)
	// chunks' finalizer would free the chunk evaluated above.
	runtime.KeepAlive(chunks)
	if rc != 0 {
		return 0, fmt.Errorf("llamacppshim: mtmd eval chunk %d failed (rc=%d)", i, int(rc))
	}
	return int(newPast), nil
}
