package contenoxcli

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var errEmptyPrompt = errors.New("aborted due to empty prompt")

func resolveEditor() string {
	if e := strings.TrimSpace(os.Getenv("VISUAL")); e != "" {
		return e
	}
	if e := strings.TrimSpace(os.Getenv("EDITOR")); e != "" {
		return e
	}
	return "nano"
}

func captureFromEditor(seed []byte, modelHint string) (string, error) {
	f, err := os.CreateTemp("", "contenox-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	initial := buildEditorTemplate(seed, modelHint)
	if _, err := f.Write(initial); err != nil {
		f.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	initialHash := sha256.Sum256(initial)

	if err := runEditor(tmpPath); err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}

	final, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read temp file: %w", err)
	}
	finalHash := sha256.Sum256(final)
	if bytes.Equal(initialHash[:], finalHash[:]) {
		return "", errEmptyPrompt
	}

	prompt := stripCommentLines(string(final))
	if strings.TrimSpace(prompt) == "" {
		return "", errEmptyPrompt
	}
	return prompt, nil
}

func buildEditorTemplate(seed []byte, modelHint string) []byte {
	var b strings.Builder
	b.WriteByte('\n')
	b.WriteString("# ---------------------------------------------------------\n")
	b.WriteString("# Write your prompt above. Lines starting with '#' are ignored.\n")
	if modelHint != "" {
		fmt.Fprintf(&b, "# Target Model: %s\n", modelHint)
	}
	b.WriteString("# ---------------------------------------------------------\n")
	if len(seed) > 0 {
		b.Write(seed)
		if !bytes.HasSuffix(seed, []byte{'\n'}) {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

func stripCommentLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func runEditor(path string) error {
	editor := resolveEditor()
	cmd := exec.Command("sh", "-c", fmt.Sprintf("%s %q", editor, path))

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		defer tty.Close()
		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty
	}
	return cmd.Run()
}
