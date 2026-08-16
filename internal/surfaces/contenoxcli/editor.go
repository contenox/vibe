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
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		return "code --wait"
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
		if len(seed) > 0 {
			// An unedited buffer with a seed means "keep the seed as-is".
			return string(seed), nil
		}
		// No seed and nothing written: there is nothing to send.
		return "", errEmptyPrompt
	}

	prompt := stripCommentLines(string(final))
	if strings.TrimSpace(prompt) == "" {
		return "", errEmptyPrompt
	}
	return prompt, nil
}

const templateBannerRule = "# ---------------------------------------------------------"

func buildEditorTemplate(seed []byte, modelHint string) []byte {
	var b strings.Builder
	if len(seed) > 0 {
		b.Write(seed)
		if !bytes.HasSuffix(seed, []byte{'\n'}) {
			b.WriteByte('\n')
		}
		b.WriteByte('\n') // boundary blank between the seed and the banner
	} else {
		b.WriteByte('\n') // blank line above the banner where typing starts
	}
	b.WriteString(templateBannerRule + "\n")
	b.WriteString("# Write your prompt above. Lines starting with '#' are ignored.\n")
	if modelHint != "" {
		fmt.Fprintf(&b, "# Target Model: %s\n", modelHint)
	}
	b.WriteString(templateBannerRule + "\n")
	return []byte(b.String())
}

func stripCommentLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line == "" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "#") {
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
