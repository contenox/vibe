//go:build openvino && openvino_genai

package ovsession

/*
#cgo CXXFLAGS: -std=c++17
#include <stdlib.h>
#include "scheduler_probe.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

const (
	schedulerReportLen = 16 * 1024
	schedulerErrLen    = 4 * 1024
)

func runGenAISchedulerProbe(modelDir, device string) (string, error) {
	cModelDir := C.CString(modelDir)
	cDevice := C.CString(device)
	defer C.free(unsafe.Pointer(cModelDir))
	defer C.free(unsafe.Pointer(cDevice))

	report := C.calloc(1, C.size_t(schedulerReportLen))
	if report == nil {
		return "", fmt.Errorf("allocate scheduler report buffer")
	}
	defer C.free(report)

	errbuf := C.calloc(1, C.size_t(schedulerErrLen))
	if errbuf == nil {
		return "", fmt.Errorf("allocate scheduler error buffer")
	}
	defer C.free(errbuf)

	rc := C.cx_genai_scheduler_probe(
		cModelDir,
		cDevice,
		(*C.char)(report),
		C.size_t(schedulerReportLen),
		(*C.char)(errbuf),
		C.size_t(schedulerErrLen),
	)
	if rc != 0 {
		return "", fmt.Errorf("%s", C.GoString((*C.char)(errbuf)))
	}
	return C.GoString((*C.char)(report)), nil
}
