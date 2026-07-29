//go:build openvino && openvino_genai

package ovsession

/*
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "genai.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"unsafe"
)

// SupportsColdKV reports whether the linked GenAI bridge exposes external KV
// import/export hooks.
func (s *GenAISession) SupportsColdKV() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return false
	}
	return C.cx_genai_supports_cold_kv(s.ptr) != 0
}

// ExportColdKV exports a logical token range from OpenVINO hot KV.
func (s *GenAISession) ExportColdKV(ctx context.Context, r ColdKVRange) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.End <= r.Start || len(r.Tokens) == 0 || len(r.PrefixTokens) == 0 {
		return nil, errors.New("openvino cold KV export range is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil, errors.New("openvino GenAI session is closed")
	}
	if C.cx_genai_supports_cold_kv(s.ptr) == 0 {
		return nil, ErrColdKVUnsupported
	}

	cTokens := make([]C.int64_t, len(r.Tokens))
	for i, tok := range r.Tokens {
		cTokens[i] = C.int64_t(tok)
	}
	cPrefixTokens := make([]C.int64_t, len(r.PrefixTokens))
	for i, tok := range r.PrefixTokens {
		cPrefixTokens[i] = C.int64_t(tok)
	}
	cHash := C.CString(r.TokenHash)
	defer C.free(unsafe.Pointer(cHash))
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return nil, errors.New("allocate OpenVINO cold KV export error buffer")
	}
	defer C.free(errbuf)

	var data *C.uint8_t
	var dataLen C.size_t
	rc := C.cx_genai_export_cold_kv(
		s.ptr,
		C.int(r.Start),
		C.int(r.End),
		(*C.int64_t)(unsafe.Pointer(&cTokens[0])),
		C.size_t(len(cTokens)),
		(*C.int64_t)(unsafe.Pointer(&cPrefixTokens[0])),
		C.size_t(len(cPrefixTokens)),
		cHash,
		&data,
		&dataLen,
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 0 {
		return nil, fmt.Errorf("openvino GenAI export cold KV: %s", C.GoString((*C.char)(errbuf)))
	}
	defer C.cx_genai_kv_data_free(unsafe.Pointer(data))
	if dataLen == 0 {
		return nil, nil
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(dataLen))
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

// ImportColdKV imports a previously exported logical token range into OpenVINO
// hot KV.
func (s *GenAISession) ImportColdKV(ctx context.Context, r ColdKVRange, kv []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.End <= r.Start || len(r.Tokens) == 0 || len(r.PrefixTokens) == 0 || len(kv) == 0 {
		return errors.New("openvino cold KV import range or payload is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return errors.New("openvino GenAI session is closed")
	}
	if C.cx_genai_supports_cold_kv(s.ptr) == 0 {
		return ErrColdKVUnsupported
	}

	cTokens := make([]C.int64_t, len(r.Tokens))
	for i, tok := range r.Tokens {
		cTokens[i] = C.int64_t(tok)
	}
	cPrefixTokens := make([]C.int64_t, len(r.PrefixTokens))
	for i, tok := range r.PrefixTokens {
		cPrefixTokens[i] = C.int64_t(tok)
	}
	cHash := C.CString(r.TokenHash)
	defer C.free(unsafe.Pointer(cHash))
	errbuf := C.calloc(1, C.size_t(genAIErrLen))
	if errbuf == nil {
		return errors.New("allocate OpenVINO cold KV import error buffer")
	}
	defer C.free(errbuf)

	rc := C.cx_genai_import_cold_kv(
		s.ptr,
		C.int(r.Start),
		C.int(r.End),
		C.int(r.DestStart),
		(*C.int64_t)(unsafe.Pointer(&cTokens[0])),
		C.size_t(len(cTokens)),
		(*C.int64_t)(unsafe.Pointer(&cPrefixTokens[0])),
		C.size_t(len(cPrefixTokens)),
		cHash,
		(*C.uint8_t)(unsafe.Pointer(&kv[0])),
		C.size_t(len(kv)),
		(*C.char)(errbuf),
		C.size_t(genAIErrLen),
	)
	if rc != 0 {
		return fmt.Errorf("openvino GenAI import cold KV: %s", C.GoString((*C.char)(errbuf)))
	}
	return nil
}
