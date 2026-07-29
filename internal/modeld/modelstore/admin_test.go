package modelstore

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func buildTestTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

func TestUnit_Admin_ListModels_MixedTypes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "llm", "model.gguf"), []byte("weights"))
	writeFile(t, filepath.Join(dir, "vision", "openvino_model.xml"), []byte("<ir/>"))

	admin := NewAdmin(dir)
	models, err := admin.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ListModels = %+v, want 2 entries", models)
	}
	byName := map[string]NodeModel{}
	for _, m := range models {
		byName[m.Name] = m
	}
	if byName["llm"].Type != "llama" || byName["llm"].Digest == "" {
		t.Fatalf("llm entry = %+v", byName["llm"])
	}
	if byName["vision"].Type != "openvino" {
		t.Fatalf("vision entry = %+v", byName["vision"])
	}
}

// TestUnit_Admin_ListModels_ContextLengthUnknownWithoutNativeBackend documents
// the expected default-build behavior of the new ContextLength enrichment:
// modeld/llama.ContextLength and modeld/openvino.ContextLength both delegate
// to a native (CGo, build-tag-gated) header parser that reports "not compiled
// in" outside a real llamacpp_direct/openvino_genai build — so ContextLength
// must stay 0 ("unknown") here, and, critically, that failure must not hide
// the model from the inventory or fail the whole scan (skip-this-model-only
// semantics, exercised for both engine types).
func TestUnit_Admin_ListModels_ContextLengthUnknownWithoutNativeBackend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "llm", "model.gguf"), []byte("weights"))
	writeFile(t, filepath.Join(dir, "vision", "openvino_model.xml"), []byte("<ir/>"))

	admin := NewAdmin(dir)
	models, err := admin.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ListModels = %+v, want both models present despite unavailable header parse", models)
	}
	for _, m := range models {
		if m.ContextLength != 0 {
			t.Fatalf("model %q ContextLength = %d, want 0 (native backend not compiled into the test binary)", m.Name, m.ContextLength)
		}
	}
}

func TestUnit_Admin_ListModels_SkipsStagingDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, stagingPrefix+"leftover-abc123"), 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	admin := NewAdmin(dir)
	models, err := admin.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ListModels = %+v, want empty (staging dir must be skipped)", models)
	}
}

func TestUnit_Admin_RemoveModel_NotFound(t *testing.T) {
	admin := NewAdmin(t.TempDir())
	err := admin.RemoveModel(context.Background(), "missing")
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("RemoveModel(missing) = %v, want ErrModelNotFound", err)
	}
}

func TestUnit_Admin_RemoveModel_DeletesDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "model.gguf"), []byte("weights"))
	admin := NewAdmin(dir)
	if err := admin.RemoveModel(context.Background(), "a"); err != nil {
		t.Fatalf("RemoveModel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("model dir still present: err=%v", err)
	}
}

func TestUnit_Admin_DiskStats_ReturnsPositiveTotal(t *testing.T) {
	admin := NewAdmin(t.TempDir())
	st, err := admin.DiskStats(context.Background())
	if err != nil {
		t.Fatalf("DiskStats: %v", err)
	}
	if st.TotalBytes <= 0 {
		t.Fatalf("TotalBytes = %d, want > 0", st.TotalBytes)
	}
}

func TestUnit_Admin_ReceiveModel_File(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	content := []byte("gguf-weights")

	res, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "a", Type: "llama", Digest: digestOf(content), Format: PushFormatFile,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("ReceiveModel: %v", err)
	}
	if res.AlreadyPresent {
		t.Fatal("first receive reported AlreadyPresent")
	}
	if res.BytesWritten != int64(len(content)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(content))
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "model.gguf"))
	if err != nil {
		t.Fatalf("read installed model: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestUnit_Admin_ReceiveModel_Tar(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	tarBytes := buildTestTar(t, map[string][]byte{
		"openvino_model.xml": []byte("<ir/>"),
		"openvino_model.bin": []byte("weights"),
	})

	res, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "vision", Type: "openvino", Digest: digestOf(tarBytes), Format: PushFormatTar,
	}, bytes.NewReader(tarBytes))
	if err != nil {
		t.Fatalf("ReceiveModel: %v", err)
	}
	if res.BytesWritten != int64(len(tarBytes)) {
		t.Fatalf("BytesWritten = %d, want %d (raw stream size, not unpacked size)", res.BytesWritten, len(tarBytes))
	}
	if _, err := os.Stat(filepath.Join(dir, "vision", "openvino_model.xml")); err != nil {
		t.Fatalf("unpacked entrypoint missing: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "vision", "openvino_model.bin"))
	if err != nil || string(got) != "weights" {
		t.Fatalf("unpacked bin content = %q, err=%v", got, err)
	}
}

// A llama vision model ships as a tar of model.gguf + mmproj.gguf so both
// files install atomically; the store must keep the pair together, report the
// combined install footprint, and resolve the projector next to the model.
func TestUnit_Admin_ReceiveModel_TarLlamaVisionKeepsBothFiles(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)

	weights := []byte("gguf-weights")
	projector := []byte("gguf-projector")
	tarBytes := buildTestTar(t, map[string][]byte{
		"model.gguf":  weights,
		"mmproj.gguf": projector,
	})
	if _, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "vlm", Type: "llama", Digest: digestOf(tarBytes), Format: PushFormatTar,
	}, bytes.NewReader(tarBytes)); err != nil {
		t.Fatalf("ReceiveModel: %v", err)
	}

	modelPath, err := Resolve(dir, "vlm", "llama", digestOf(weights))
	if err != nil {
		t.Fatalf("Resolve after tar install: %v", err)
	}
	mmproj := ResolveMMProj(modelPath)
	if want := filepath.Join(dir, "vlm", "mmproj.gguf"); mmproj != want {
		t.Fatalf("ResolveMMProj = %q, want %q", mmproj, want)
	}

	models, err := admin.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Name != "vlm" || models[0].Type != "llama" {
		t.Fatalf("ListModels = %+v", models)
	}
	if want := int64(len(weights) + len(projector)); models[0].SizeBytes != want {
		t.Fatalf("SizeBytes = %d, want combined footprint %d", models[0].SizeBytes, want)
	}
	if models[0].Digest != digestOf(weights) {
		t.Fatalf("Digest = %q, want model.gguf digest %q (projector must not change cache identity)", models[0].Digest, digestOf(weights))
	}
}

func TestUnit_Admin_ReceiveModel_DigestMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	content := []byte("gguf-weights")

	_, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "a", Type: "llama", Digest: "wrong-digest", Format: PushFormatFile,
	}, bytes.NewReader(content))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("ReceiveModel = %v, want ErrDigestMismatch", err)
	}
	// The rejected push must not leave a model installed, and must not leave
	// its staging directory behind.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("dir entries after rejected push = %v, want none", entries)
	}
}

func TestUnit_Admin_ReceiveModel_UnsupportedTypeRejected(t *testing.T) {
	admin := NewAdmin(t.TempDir())
	_, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "a", Type: "ollama", Format: PushFormatFile,
	}, bytes.NewReader([]byte("x")))
	if !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("ReceiveModel = %v, want ErrUnsupportedType", err)
	}
}

func TestUnit_Admin_ReceiveModel_AlreadyPresentDiscardsStaging(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	content := []byte("gguf-weights")
	manifest := PushManifest{Name: "a", Type: "llama", Digest: digestOf(content), Format: PushFormatFile}

	if _, err := admin.ReceiveModel(context.Background(), manifest, bytes.NewReader(content)); err != nil {
		t.Fatalf("first ReceiveModel: %v", err)
	}
	res, err := admin.ReceiveModel(context.Background(), manifest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second ReceiveModel: %v", err)
	}
	if !res.AlreadyPresent {
		t.Fatal("repeat push with matching digest did not report AlreadyPresent")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Fatalf("staging dir left behind after idempotent push: %s", e.Name())
		}
	}
}

// blockingReader blocks the first Read until release is closed, then yields
// content. Used to hold ReceiveModel mid-flight to exercise the per-model
// concurrency guard deterministically (no sleep-based timing).
type blockingReader struct {
	content []byte
	release <-chan struct{}
	read    bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.read {
		<-r.release
		r.read = true
	}
	if len(r.content) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.content)
	r.content = r.content[n:]
	return n, nil
}

func TestUnit_Admin_ReceiveModel_ConcurrentPushSameNameRejected(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	content := []byte("gguf-weights")
	release := make(chan struct{})

	firstDone := make(chan error, 1)
	go func() {
		_, err := admin.ReceiveModel(context.Background(), PushManifest{
			Name: "a", Type: "llama", Digest: digestOf(content), Format: PushFormatFile,
		}, &blockingReader{content: content, release: release})
		firstDone <- err
	}()

	// Wait for the first push to actually claim the name before firing the
	// second: claim() happens before the blocking read, so poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		admin.mu.Lock()
		_, busy := admin.inFlight["a"]
		admin.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first push never claimed the model name")
		}
		time.Sleep(time.Millisecond)
	}

	_, err := admin.ReceiveModel(context.Background(), PushManifest{
		Name: "a", Type: "llama", Digest: digestOf(content), Format: PushFormatFile,
	}, bytes.NewReader(content))
	if err == nil {
		t.Fatal("concurrent push for the same name = nil error, want rejection")
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first push: %v", err)
	}
}

func TestUnit_Admin_ReceiveModel_DifferentNamesConcurrentlyAllowed(t *testing.T) {
	dir := t.TempDir()
	admin := NewAdmin(dir)
	contentA := []byte("weights-a")
	contentB := []byte("weights-b")

	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() {
		_, err := admin.ReceiveModel(context.Background(), PushManifest{
			Name: "a", Type: "llama", Digest: digestOf(contentA), Format: PushFormatFile,
		}, bytes.NewReader(contentA))
		doneA <- err
	}()
	go func() {
		_, err := admin.ReceiveModel(context.Background(), PushManifest{
			Name: "b", Type: "llama", Digest: digestOf(contentB), Format: PushFormatFile,
		}, bytes.NewReader(contentB))
		doneB <- err
	}()

	if err := <-doneA; err != nil {
		t.Fatalf("push a: %v", err)
	}
	if err := <-doneB; err != nil {
		t.Fatalf("push b: %v", err)
	}
}
