//go:build openvino && openvino_legacy_shim

package ovsession

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSystem_OpenVINOSession_SnapshotRoundTripFreshSession(t *testing.T) {
	modelDir := getenv("CONTENOX_OPENVINO_TEST_MODEL", "")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR model directory")
	}
	device := getenv("CONTENOX_OPENVINO_TEST_DEVICE", "CPU")

	prompt := []int64{785, 6722, 374, 264, 7522}
	snapshot := filepath.Join(t.TempDir(), "kv.snapshot")

	a, err := New(modelDir, device)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Prefill(prompt); err != nil {
		t.Fatal(err)
	}
	if err := a.SnapshotSave(snapshot); err != nil {
		t.Fatal(err)
	}
	seqA := decodeN(t, a, 8)

	b, err := New(modelDir, device)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SnapshotRestore(snapshot); err != nil {
		t.Fatal(err)
	}
	seqB := decodeN(t, b, 8)

	if !reflect.DeepEqual(seqA, seqB) {
		t.Fatalf("restored session diverged\nA=%v\nB=%v", seqA, seqB)
	}
}

func TestSystem_OpenVINOSession_SnapshotDataRoundTripFreshSession(t *testing.T) {
	modelDir := getenv("CONTENOX_OPENVINO_TEST_MODEL", "")
	if modelDir == "" {
		t.Skip("set CONTENOX_OPENVINO_TEST_MODEL to an OpenVINO IR model directory")
	}
	device := getenv("CONTENOX_OPENVINO_TEST_DEVICE", "CPU")

	prompt := []int64{785, 6722, 374, 264, 7522}

	a, err := New(modelDir, device)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Prefill(prompt); err != nil {
		t.Fatal(err)
	}
	data, err := a.SnapshotData()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("snapshot data is empty")
	}
	seqA := decodeN(t, a, 8)

	b, err := New(modelDir, device)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SnapshotRestoreData(data); err != nil {
		t.Fatal(err)
	}
	seqB := decodeN(t, b, 8)

	if !reflect.DeepEqual(seqA, seqB) {
		t.Fatalf("restored session diverged\nA=%v\nB=%v", seqA, seqB)
	}
}

func decodeN(t *testing.T, s *Session, n int) []int64 {
	t.Helper()
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, err := s.DecodeNext()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
