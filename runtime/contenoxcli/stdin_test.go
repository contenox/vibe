package contenoxcli

import (
	"os"
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

func TestReadStdinIfAvailableSkipsIdlePipe(t *testing.T) {
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

func TestResolveRunInputCombinesArgsAndReadyStdin(t *testing.T) {
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
