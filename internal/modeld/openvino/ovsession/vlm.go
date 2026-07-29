//go:build openvino && openvino_genai

package ovsession

/*
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "vlm.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/contenox/contenox/internal/modeld/internal/sessionkit"
)

// VLMAvailable reports whether the OpenVINO VLM (vision) backend was built.
const VLMAvailable = true

// VLMImage is one encoded image file attached to a VLM generation: the raw
// bytes of a PNG/JPEG/BMP/... file, decoded natively by the cell (stb_image).
// MimeType is advisory; the decoder sniffs the actual format from the bytes.
// It mirrors transport.ImagePart.
type VLMImage struct {
	Data     []byte
	MimeType string
}

// ProbeVLMImage validates that image bytes decode to a supported raster
// format, returning the dimensions. It needs no model and is the cheap
// reject-early check for attachments.
func ProbeVLMImage(data []byte) (width, height int, err error) {
	if len(data) == 0 {
		return 0, 0, errors.New("openvino VLM image bytes are empty")
	}
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return 0, 0, errors.New("allocate OpenVINO VLM error buffer")
	}
	defer C.free(errbuf)

	var w, h C.int
	rc := C.cx_vlm_image_probe(
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)),
		&w,
		&h,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 0 {
		return 0, 0, fmt.Errorf("openvino VLM image probe: %s", C.GoString((*C.char)(errbuf)))
	}
	return int(w), int(h), nil
}

// VLMSession wraps an ov::genai::VLMPipeline. It is the vision counterpart of
// GenAISession, restricted to the pipeline's PUBLIC surface: no token-tape
// prefill, no prefix-cache warm-up, no cold-KV — every generation re-prefills
// the full multimodal prompt (see vlm.cpp).
type VLMSession struct {
	mu  sync.Mutex
	ptr *C.cx_vlm_session
}

// NewVLM creates an OpenVINO GenAI VLMPipeline session for the exported VLM
// IR directory at modelDir.
func NewVLM(modelDir string, device string) (*VLMSession, error) {
	if modelDir == "" {
		return nil, errors.New("openvino VLM model directory is required")
	}
	if device == "" {
		device = "CPU"
	}

	cDir := C.CString(modelDir)
	cDev := C.CString(device)
	defer C.free(unsafe.Pointer(cDir))
	defer C.free(unsafe.Pointer(cDev))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO VLM error buffer")
	}
	defer C.free(errbuf)

	ptr := C.cx_vlm_session_new(cDir, cDev, (*C.char)(errbuf), C.size_t(genAIErrLen))
	if ptr == nil {
		return nil, fmt.Errorf("openvino VLM session new: %s", C.GoString((*C.char)(errbuf)))
	}

	s := &VLMSession{ptr: ptr}
	runtime.SetFinalizer(s, (*VLMSession).mustClose)
	return s, nil
}

// ApplyChatTemplate renders role/content turns with the model's own original
// chat template (captured before the pipeline template is overridden with the
// identity template; see vlm.cpp). The rendered prompt may contain universal
// <ov_genai_image_i> tags marking image positions.
func (s *VLMSession) ApplyChatTemplate(messages []ChatMessage, addGenerationPrompt bool) (string, error) {
	if len(messages) == 0 {
		return "", errors.New("openvino VLM chat template requires at least one message")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return "", errors.New("openvino VLM session is closed")
	}

	roles := make([]*C.char, len(messages))
	contents := make([]*C.char, len(messages))
	for i, m := range messages {
		roles[i] = C.CString(m.Role)
		contents[i] = C.CString(m.Content)
	}
	defer func() {
		for i := range messages {
			C.free(unsafe.Pointer(roles[i]))
			C.free(unsafe.Pointer(contents[i]))
		}
	}()

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return "", errors.New("allocate OpenVINO VLM error buffer")
	}
	defer C.free(errbuf)

	var out *C.char
	var outLen C.size_t
	rc := C.cx_vlm_apply_chat_template(
		s.ptr,
		(**C.char)(unsafe.Pointer(&roles[0])),
		(**C.char)(unsafe.Pointer(&contents[0])),
		C.size_t(len(messages)),
		cbool(addGenerationPrompt),
		&out,
		&outLen,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	defer freeCData(unsafe.Pointer(out))
	if rc != 0 {
		return "", fmt.Errorf("openvino VLM apply chat template: %s", C.GoString((*C.char)(errbuf)))
	}
	return cAllocatedText(out, outLen), nil
}

// Tokenize encodes prompt text with the VLM's tokenizer. Image placeholders
// are not expanded here — this counts TEXT tokens only; vision tokens are
// estimated separately (ModelInfo.VisionTokensPerImage).
func (s *VLMSession) Tokenize(ctx context.Context, prompt string, addSpecial bool) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, errors.New("openvino VLM tokenization prompt is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil, errors.New("openvino VLM session is closed")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO VLM tokenization error buffer")
	}
	defer C.free(errbuf)

	var required C.size_t
	rc := C.cx_vlm_tokenize(
		s.ptr,
		cPrompt,
		cbool(addSpecial),
		nil,
		0,
		&required,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 2 && rc != 0 {
		return nil, fmt.Errorf("openvino VLM tokenize: %s", C.GoString((*C.char)(errbuf)))
	}
	if required == 0 {
		return nil, nil
	}

	buf := make([]C.int64_t, int(required))
	rc = C.cx_vlm_tokenize(
		s.ptr,
		cPrompt,
		cbool(addSpecial),
		(*C.int64_t)(unsafe.Pointer(&buf[0])),
		required,
		&required,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 0 {
		return nil, fmt.Errorf("openvino VLM tokenize: %s", C.GoString((*C.char)(errbuf)))
	}
	out := make([]int, len(buf))
	for i, tok := range buf {
		out[i] = int(tok)
	}
	return out, nil
}

// Stream runs one already-templated prompt (universal <ov_genai_image_i> tags
// mark image positions) with its images and returns decoded text deltas as
// the pipeline produces them.
func (s *VLMSession) Stream(ctx context.Context, prompt string, images []VLMImage, opts GenerateOptions) (<-chan StreamChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, errors.New("openvino VLM prompt is required")
	}
	for i, img := range images {
		if len(img.Data) == 0 {
			return nil, fmt.Errorf("openvino VLM image %d is empty", i)
		}
	}

	s.mu.Lock()
	if s.ptr == nil {
		s.mu.Unlock()
		return nil, errors.New("openvino VLM session is closed")
	}
	ptr := s.ptr

	stream := C.cx_vlm_stream_new()
	if stream == nil {
		s.mu.Unlock()
		return nil, errors.New("allocate OpenVINO VLM stream")
	}

	ch := make(chan StreamChunk, 16)
	generatorDone := make(chan struct{})

	go func() {
		defer close(generatorDone)
		defer s.mu.Unlock()

		cPrompt := C.CString(prompt)
		defer C.free(unsafe.Pointer(cPrompt))

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			msg := C.CString("allocate OpenVINO VLM stream generator error buffer")
			C.cx_vlm_stream_abort(stream, msg)
			C.free(unsafe.Pointer(msg))
			C.cx_vlm_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		// The image structs and their byte payloads are copied into C memory:
		// cgo forbids passing a Go-allocated struct array that itself contains
		// Go pointers, and the C-side worker thread reads the buffers for the
		// duration of the call.
		var cImages *C.cx_vlm_image
		if len(images) > 0 {
			arr := C.calloc(C.size_t(len(images)), C.sizeof_cx_vlm_image)
			if arr == nil {
				msg := C.CString("allocate OpenVINO VLM image array")
				C.cx_vlm_stream_abort(stream, msg)
				C.free(unsafe.Pointer(msg))
				return
			}
			defer C.free(arr)
			slots := unsafe.Slice((*C.cx_vlm_image)(arr), len(images))
			for i, img := range images {
				data := C.CBytes(img.Data)
				defer C.free(data)
				slots[i].data = (*C.uint8_t)(data)
				slots[i].len = C.size_t(len(img.Data))
				if img.MimeType != "" {
					mime := C.CString(img.MimeType)
					defer C.free(unsafe.Pointer(mime))
					slots[i].mime = mime
				}
			}
			cImages = (*C.cx_vlm_image)(arr)
		}

		var temp C.float
		var useTemp C.int
		if opts.Temperature != nil {
			temp = C.float(*opts.Temperature)
			useTemp = 1
		}
		var topP C.float
		var useTopP C.int
		if opts.TopP != nil {
			topP = C.float(*opts.TopP)
			useTopP = 1
		}
		var topK C.size_t
		var useTopK C.int
		if opts.TopK != nil && *opts.TopK > 0 {
			topK = C.size_t(*opts.TopK)
			useTopK = 1
		}
		var seed C.size_t
		var useSeed C.int
		if opts.Seed != nil && *opts.Seed >= 0 {
			seed = C.size_t(*opts.Seed)
			useSeed = 1
		}

		done := make(chan struct{})
		if ctx.Done() != nil {
			go func() {
				select {
				case <-ctx.Done():
					C.cx_vlm_session_cancel(ptr)
				case <-done:
				}
			}()
		}
		C.cx_vlm_generate_stream(
			ptr,
			cPrompt,
			cImages,
			C.size_t(len(images)),
			C.size_t(max(opts.MaxNewTokens, 0)),
			temp,
			useTemp,
			topP,
			useTopP,
			topK,
			useTopK,
			seed,
			useSeed,
			stream,
			(*C.char)(errbuf),
			C.size_t(genAIErrLen),
		)
		close(done)
	}()

	go func() {
		defer close(ch)
		defer func() {
			<-generatorDone
			C.cx_vlm_stream_free(stream)
		}()

		errbuf := C.calloc(1, C.size_t(genAIErrLen))
		if errbuf == nil {
			_ = sessionkit.Send(ctx, ch, StreamChunk{Error: errors.New("allocate OpenVINO VLM stream error buffer")})
			C.cx_vlm_session_cancel(ptr)
			return
		}
		defer C.free(errbuf)

		for {
			var out *C.char
			var outLen C.size_t
			rc := C.cx_vlm_stream_next(
				stream,
				&out,
				&outLen,
				(*C.char)(errbuf),
				C.size_t(genAIErrLen),
			)
			text := cAllocatedText(out, outLen)
			freeCData(unsafe.Pointer(out))
			switch rc {
			case 0:
				if text == "" {
					continue
				}
				select {
				case ch <- StreamChunk{Text: text}:
				case <-ctx.Done():
					C.cx_vlm_session_cancel(ptr)
					sessionkit.TrySend(ch, StreamChunk{Error: ctx.Err()})
					return
				}
			case 1:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				}
				return
			case 3:
				if err := ctx.Err(); err != nil {
					sendTerminalStreamError(ctx, ch, err)
				} else {
					sendTerminalStreamError(ctx, ch, errors.New("openvino VLM generation canceled"))
				}
				return
			default:
				sendTerminalStreamError(ctx, ch, fmt.Errorf("openvino VLM stream: %s", C.GoString((*C.char)(errbuf))))
				return
			}
		}
	}()

	return ch, nil
}

// Generate runs one prompt with images to completion and returns the full
// text (a convenience over Stream for callers that need whole outputs).
func (s *VLMSession) Generate(ctx context.Context, prompt string, images []VLMImage, opts GenerateOptions) (string, error) {
	ch, err := s.Stream(ctx, prompt, images, opts)
	if err != nil {
		return "", err
	}
	var text string
	for chunk := range ch {
		if chunk.Error != nil {
			return text, chunk.Error
		}
		text += chunk.Text
	}
	return text, nil
}

// Close releases the native VLM session.
func (s *VLMSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil
	}
	runtime.SetFinalizer(s, nil)
	C.cx_vlm_session_free(s.ptr)
	s.ptr = nil
	return nil
}

func (s *VLMSession) mustClose() {
	_ = s.Close()
}
