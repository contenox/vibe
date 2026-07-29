//go:build openvino && openvino_genai

package ovsession

/*
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "embed.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// EmbedSession wraps an ov::genai::TextEmbeddingPipeline.
type EmbedSession struct {
	mu  sync.Mutex
	ptr *C.cx_embed_session
}

// NewEmbed creates an OpenVINO GenAI TextEmbeddingPipeline session.
func NewEmbed(modelDir string, device string) (*EmbedSession, error) {
	if modelDir == "" {
		return nil, errors.New("openvino Embed model directory is required")
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
		return nil, errors.New("allocate OpenVINO Embed error buffer")
	}
	defer C.free(errbuf)

	ptr := C.cx_embed_session_new(cDir, cDev, (*C.char)(errbuf), C.size_t(genAIErrLen))
	if ptr == nil {
		return nil, fmt.Errorf("openvino Embed session new: %s", C.GoString((*C.char)(errbuf)))
	}

	s := &EmbedSession{ptr: ptr}
	runtime.SetFinalizer(s, (*EmbedSession).mustClose)
	return s, nil
}

// Embed generates embeddings for a single prompt.
func (s *EmbedSession) Embed(ctx context.Context, prompt string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prompt == "" {
		return nil, errors.New("openvino Embed prompt is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil, errors.New("openvino Embed session is closed")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	// Typically embeddings are <= 8192 floats
	const maxEmbedLen = 8192
	out := C.calloc(maxEmbedLen, C.sizeof_float)
	if out == nil {
		return nil, errors.New("allocate OpenVINO Embed output buffer")
	}
	defer C.free(out)

	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO Embed error buffer")
	}
	defer C.free(errbuf)

	var outWritten C.size_t

	done := make(chan struct{})
	// No native cancel for TextEmbeddingPipeline generate(), but we listen anyway
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				// Future: handle cancel if supported
			case <-done:
			}
		}()
	}

	rc := C.cx_embed_embed(
		s.ptr,
		cPrompt,
		(*C.float)(out),
		maxEmbedLen,
		&outWritten,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	close(done)

	if rc != 0 {
		return nil, fmt.Errorf("openvino Embed generate: %s", C.GoString((*C.char)(errbuf)))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Copy to Go slice
	n := int(outWritten)
	res := make([]float32, n)
	cArray := (*[maxEmbedLen]C.float)(out)
	for i := 0; i < n; i++ {
		res[i] = float32(cArray[i])
	}

	return res, nil
}

// Close releases the native Embed session.
func (s *EmbedSession) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil
	}
	runtime.SetFinalizer(s, nil)
	C.cx_embed_session_free(s.ptr)
	s.ptr = nil
	return nil
}

func (s *EmbedSession) mustClose() {
	_ = s.Close()
}
