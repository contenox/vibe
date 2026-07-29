//go:build llamacpp_direct

package llamacppshim

import (
	"os"
	"testing"
)

func TestDirectShimMTMDMediaMarker(t *testing.T) {
	if got := MediaMarker(); got != "<__media__>" {
		t.Fatalf("MediaMarker() = %q, want %q", got, "<__media__>")
	}
}

func TestDirectShimMTMDProjCapsUnreadable(t *testing.T) {
	if vision, audio := MMProjCaps(""); vision || audio {
		t.Fatalf("MMProjCaps(empty) = vision=%v audio=%v, want false/false", vision, audio)
	}
	if vision, audio := MMProjCaps("/nonexistent/mmproj.gguf"); vision || audio {
		t.Fatalf("MMProjCaps(missing) = vision=%v audio=%v, want false/false", vision, audio)
	}
	// A structurally valid GGUF without projector metadata must not certify any
	// input modality.
	path := writeShimTestGGUF(t, nil)
	if vision, audio := MMProjCaps(path); vision || audio {
		t.Fatalf("MMProjCaps(non-projector gguf) = vision=%v audio=%v, want false/false", vision, audio)
	}
}

func TestDirectShimMTMDInitFailsCleanly(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	if modelPath == "" {
		t.Skip("set CONTENOX_LLAMA_TINY_GGUF to test mtmd projector init failure paths")
	}
	model, err := LoadModel(modelPath, ModelConfig{UseMmap: true})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()

	if _, err := NewMTMDContext(model, "/nonexistent/mmproj.gguf", MTMDConfig{}); err == nil {
		t.Fatal("NewMTMDContext with a missing mmproj path succeeded, want error")
	}
	// A text-model GGUF is not a projector; init must fail, not crash.
	if _, err := NewMTMDContext(model, modelPath, MTMDConfig{}); err == nil {
		t.Fatal("NewMTMDContext with a non-projector GGUF succeeded, want error")
	}
}
