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
		if len(seed) > 0 {
			// Exiting without touching the buffer is not "empty" when a seed
			// was provided — it means "keep the seed as-is". beam: Ctrl+E
			// then :q must not destroy the draft under a false empty-prompt
			// message. chat -e: an unedited piped-stdin seed means "send the
			// piped content". Echo the original seed back verbatim; do not
			// re-derive it from the template, or a stripping bug could
			// reintroduce scaffolding on every round trip.
			return string(seed), nil
		}
		// No seed and nothing written: there is genuinely nothing to send.
		return "", errEmptyPrompt
	}

	prompt := stripCommentLines(string(final))
	if strings.TrimSpace(prompt) == "" {
		return "", errEmptyPrompt
	}
	return prompt, nil
}

// templateBannerRule is the horizontal-rule comment line that fences the
// instructional banner, top and bottom.
const templateBannerRule = "# ---------------------------------------------------------"

// buildEditorTemplate lays out the scratch file handed to $EDITOR.
//
// With a seed (beam's Ctrl+E carrying the current draft, or chat -e's piped
// stdin), the seed is the first thing in the file — above the banner — so
// the editor's cursor (which opens at the top of the file) lands on it and
// the "write your prompt above" instruction is literally true. A single
// blank line separates the seed from the banner below it.
//
// With no seed, the shape is unchanged from before: a blank line above the
// banner is where the cursor lands, ready for the operator to type into.
//
// stripCommentLines() is this function's exact inverse for the scaffolding
// it inserts: the banner block plus the one blank line that sits against it.
// Keep the two in sync — anything added here that isn't a '#'-prefixed line
// or that single boundary blank will leak into the returned prompt and, on
// a repeated round trip (seed in, edited draft out, fed back in as the next
// seed), accumulate.
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

// stripCommentLines drops the template's own scaffolding from an edited
// buffer: every '#'-prefixed banner line, plus exactly the one blank line
// that sits directly against a comment block (the boundary blank
// buildEditorTemplate inserts immediately before the banner, whether that
// puts it at the very top of the file with no seed, or between the seed and
// the banner with one).
//
// The trim is bounded to that single adjacent line on purpose: only a blank
// line immediately followed by a comment line is scaffolding. Any other
// blank line — a leading blank the operator typed on purpose, a paragraph
// break in the middle of their prompt — is indistinguishable from real
// content and must survive untouched. Without this bound, a draft that
// legitimately starts with a blank line would get eaten, and conversely a
// looser trim (e.g. TrimLeft of all leading whitespace) would still leave
// the boundary blank free to accumulate one extra newline per round trip
// when the seed is re-fed through this same template.
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
