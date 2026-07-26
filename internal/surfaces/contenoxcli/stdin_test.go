package contenoxcli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func withTestStdin(t *testing.T, stdin *os.File) {
	t.Helper()
	orig := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = orig
	})
}

func TestUnit_ReadStdinIfAvailableSkipsIdlePipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	withTestStdin(t, r)

	data, ok, err := readStdinIfAvailable(maxCLIStdinBytes)
	if err != nil {
		t.Fatalf("readStdinIfAvailable: %v", err)
	}
	if ok {
		t.Fatalf("expected stdin to be treated as empty, got ok=%v data=%q", ok, data)
	}
	if data != "" {
		t.Fatalf("expected no stdin data, got %q", data)
	}
}

func TestUnit_ResolveRunInputCombinesArgsAndReadyStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	withTestStdin(t, r)
	t.Cleanup(func() {
		_ = r.Close()
	})
	if _, err := w.WriteString("diff body"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()

	cmd := &cobra.Command{}
	cmd.Flags().String("input", "", "")

	got, err := resolveRunInput(cmd, []string{"suggest", "message"})
	if err != nil {
		t.Fatalf("resolveRunInput: %v", err)
	}
	want := "suggest message\n\ndiff body"
	if got != want {
		t.Fatalf("unexpected input:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestUnit_ResolveInputFlagValueExpandsAtFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("file prompt\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	got, err := resolveInputFlagValue("--input", "@"+path)
	if err != nil {
		t.Fatalf("resolveInputFlagValue: %v", err)
	}
	if got != "file prompt\n" {
		t.Fatalf("input = %q, want file contents", got)
	}
}

func TestUnit_ResolveInputFlagValueReportsMissingAtFile(t *testing.T) {
	_, err := resolveInputFlagValue("--input", "@"+filepath.Join(t.TempDir(), "missing.txt"))
	if err == nil {
		t.Fatal("resolveInputFlagValue error = nil, want missing-file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}
