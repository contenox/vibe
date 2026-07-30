package llama

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/modeld/capacity"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
	"github.com/contenox/contenox/libtransport"
)

func TestUnit_MMProjPathFor(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := MMProjPathFor(modelPath); got != "" {
		t.Fatalf("text-only model resolved projector %q, want empty", got)
	}
	if got := MMProjPathFor(""); got != "" {
		t.Fatalf("empty model path resolved projector %q, want empty", got)
	}
	mmproj := filepath.Join(dir, MMProjFilename)
	if err := os.WriteFile(mmproj, []byte("projector"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := MMProjPathFor(modelPath); got != mmproj {
		t.Fatalf("got %q, want %q", got, mmproj)
	}
}

func TestUnit_VisionTokensPerImageEstimate(t *testing.T) {
	tests := []struct {
		name    string
		profile llamacppshim.MMProjProfile
		want    int
	}{
		{"full patch grid", llamacppshim.MMProjProfile{ImageSize: 512, PatchSize: 16}, 1024},
		{"pixel shuffle merge", llamacppshim.MMProjProfile{ImageSize: 512, PatchSize: 16, ProjScaleFactor: 3}, 113},
		{"scale factor one is identity", llamacppshim.MMProjProfile{ImageSize: 384, PatchSize: 16, ProjScaleFactor: 1}, 576},
		{"missing image size", llamacppshim.MMProjProfile{PatchSize: 16}, 0},
		{"missing patch size", llamacppshim.MMProjProfile{ImageSize: 512}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := visionTokensPerImageEstimate(tt.profile); got != tt.want {
				t.Fatalf("visionTokensPerImageEstimate(%+v) = %d, want %d", tt.profile, got, tt.want)
			}
		})
	}
}

// withMMProjSeams installs test doubles for the projector metadata reads and
// writes a projector file next to the test model.
func withMMProjSeams(t *testing.T, modelPath string, projectorBytes int, vision bool, profile llamacppshim.MMProjProfile) {
	t.Helper()
	mmproj := filepath.Join(filepath.Dir(modelPath), MMProjFilename)
	if err := os.WriteFile(mmproj, make([]byte, projectorBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCaps, oldProfile := mmprojCaps, inspectMMProjProfile
	mmprojCaps = func(path string) (bool, bool) {
		if path != mmproj {
			t.Fatalf("mmprojCaps path = %q, want %q", path, mmproj)
		}
		return vision, false
	}
	inspectMMProjProfile = func(path string) (llamacppshim.MMProjProfile, error) {
		return profile, nil
	}
	t.Cleanup(func() { mmprojCaps, inspectMMProjProfile = oldCaps, oldProfile })
}

func TestUnit_ServiceDescribeCertifiesVisionAndBudgetsProjector(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_VISION_COMPUTE_RESERVE", "")
	path := writeTestGGUF(t, 32768)
	const projectorBytes = 1 << 20
	withMMProjSeams(t, path, projectorBytes, true, llamacppshim.MMProjProfile{ImageSize: 512, PatchSize: 16, ProjScaleFactor: 3})

	svc := NewService(
		WithMemorySource(staticMemory(8<<30)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)
	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 4096, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !info.SupportsVision {
		t.Fatalf("SupportsVision = false, want certified vision: %+v", info)
	}
	if info.VisionTokensPerImage != 113 {
		t.Fatalf("VisionTokensPerImage = %d, want 113", info.VisionTokensPerImage)
	}
	// No accelerator in this snapshot, so the whole overhead is the vision
	// term: projector weights plus the encoder compute reserve.
	if want := int64(projectorBytes) + defaultVisionEncoderReserveBytes; info.OverheadBytes != want {
		t.Fatalf("OverheadBytes = %d, want projector+encoder reserve %d", info.OverheadBytes, want)
	}
}

func TestUnit_ServiceDescribeVisionOverheadShrinksContextBudget(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_VISION_COMPUTE_RESERVE", "2MiB")
	path := writeTestGGUF(t, 32768)

	svc := func() *Service {
		return NewService(
			WithMemorySource(staticMemory(16<<20)),
			WithHostMemorySource(staticMemory(0)),
			WithCapacityPolicy(capacity.Policy{MaxResidentBytes: 12 << 20, HeadroomFrac: 0.1}),
		)
	}
	base, err := svc().Describe(t.Context(), transport.OpenSessionRequest{
		Type: "llama", Path: path, Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe text-only: %v", err)
	}

	withMMProjSeams(t, path, 1<<20, true, llamacppshim.MMProjProfile{ImageSize: 512, PatchSize: 16})
	withVision, err := svc().Describe(t.Context(), transport.OpenSessionRequest{
		Type: "llama", Path: path, Config: transport.Config{KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe vision: %v", err)
	}
	if withVision.OverheadBytes != base.OverheadBytes+(1<<20)+(2<<20) {
		t.Fatalf("OverheadBytes = %d, want base %d + projector 1MiB + reserve 2MiB", withVision.OverheadBytes, base.OverheadBytes)
	}
	if withVision.EffectiveContext >= base.EffectiveContext {
		t.Fatalf("EffectiveContext = %d, want below text-only %d (projector must eat KV budget, not spill)",
			withVision.EffectiveContext, base.EffectiveContext)
	}
}

func TestUnit_ServiceDescribeDoesNotCertifyVisionWithoutProjectorCaps(t *testing.T) {
	t.Setenv("CONTENOX_LLAMA_VISION_COMPUTE_RESERVE", "")
	path := writeTestGGUF(t, 32768)
	withMMProjSeams(t, path, 1<<20, false, llamacppshim.MMProjProfile{ImageSize: 512, PatchSize: 16})

	svc := NewService(
		WithMemorySource(staticMemory(8<<30)),
		WithHostMemorySource(staticMemory(0)),
		WithCapacityPolicy(capacity.Policy{HeadroomFrac: 0.1}),
	)
	info, err := svc.Describe(t.Context(), transport.OpenSessionRequest{
		Type:   "llama",
		Path:   path,
		Config: transport.Config{NumCtx: 4096, KVCacheType: "f16"},
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.SupportsVision || info.VisionTokensPerImage != 0 {
		t.Fatalf("uncertified projector must not report vision: %+v", info)
	}
	// The file still loads with the session, so it still costs budget.
	if want := int64(1<<20) + defaultVisionEncoderReserveBytes; info.OverheadBytes != want {
		t.Fatalf("OverheadBytes = %d, want %d", info.OverheadBytes, want)
	}
}
