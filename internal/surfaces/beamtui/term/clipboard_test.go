package term

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUnit_ClipboardPlainOSC52(t *testing.T) {
	got, truncated := clipboardSequence("hello", false, false)
	if want := "\x1b]52;c;aGVsbG8=\a"; got != want {
		t.Fatalf("OSC 52\n got %q\nwant %q", got, want)
	}
	if truncated {
		t.Fatal("short payload reported as truncated")
	}
}

func TestUnit_ClipboardTmuxPassthroughDoublesEscapes(t *testing.T) {
	got, _ := clipboardSequence("hello", true, false)
	want := "\x1bPtmux;" + "\x1b\x1b]52;c;aGVsbG8=\a" + "\x1b\\"
	if got != want {
		t.Fatalf("tmux passthrough\n got %q\nwant %q", got, want)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(got, "\x1bPtmux;"), "\x1b\\")
	if strings.Contains(strings.ReplaceAll(body, "\x1b\x1b", ""), "\x1b") {
		t.Fatalf("body carries an undoubled ESC: %q", body)
	}
}

func TestUnit_ClipboardTmuxWinsOverScreen(t *testing.T) {
	got, _ := clipboardSequence("hello", true, true)
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Fatalf("nested multiplexers must address the outer one first: %q", got)
	}
}

func TestUnit_ClipboardScreenChunksDCS(t *testing.T) {
	text := strings.Repeat("x", 4096)
	got, _ := clipboardSequence(text, false, true)

	var payload strings.Builder
	rest := got
	chunks := 0
	for rest != "" {
		if !strings.HasPrefix(rest, "\x1bP") {
			t.Fatalf("chunk %d does not open with DCS: %q", chunks, rest[:min(16, len(rest))])
		}
		rest = rest[len("\x1bP"):]
		end := strings.Index(rest, "\x1b\\")
		if end < 0 {
			t.Fatalf("chunk %d is unterminated", chunks)
		}
		// screen counts the whole sequence, so the test measures the whole
		// sequence: introducer + body + terminator, exactly as it goes out.
		onWire := len("\x1bP") + end + len("\x1b\\")
		if onWire > screenChunkSize {
			t.Fatalf("chunk %d is %d bytes on the wire, want <= %d", chunks, onWire, screenChunkSize)
		}
		if chunks == 0 && onWire != screenChunkSize {
			t.Fatalf("first chunk is %d bytes on the wire, want the full %d-byte budget used", onWire, screenChunkSize)
		}
		payload.WriteString(rest[:end])
		rest = rest[end+len("\x1b\\"):]
		chunks++
	}
	if chunks < 2 {
		t.Fatalf("payload of %d bytes produced %d chunks, want it split", len(text), chunks)
	}
	want, _ := clipboardSequence(text, false, false)
	if payload.String() != want {
		t.Fatal("reassembled chunks do not reproduce the OSC 52 sequence")
	}
}

func TestUnit_ClipboardTruncatesAtTheCap(t *testing.T) {
	// A leading ASCII byte pushes the cut inside a multibyte rune.
	text := "a" + strings.Repeat("日", 30000)
	got, truncated := clipboardSequence(text, false, false)
	if !truncated {
		t.Fatal("oversized payload not reported as truncated")
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), "\a")
	if len(payload) > clipboardPayloadCap {
		t.Fatalf("payload is %d bytes, want <= %d", len(payload), clipboardPayloadCap)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if !utf8.Valid(decoded) {
		t.Fatal("truncation split a rune")
	}
	if !strings.HasPrefix(text, string(decoded)) {
		t.Fatal("truncated copy is not a prefix of the source")
	}
	if len(decoded) < clipboardSourceCap-4 {
		t.Fatalf("truncated to %d bytes, want close to the %d-byte budget", len(decoded), clipboardSourceCap)
	}
}

func TestUnit_ClipboardKeepsPayloadsUnderTheCapIntact(t *testing.T) {
	text := strings.Repeat("y", clipboardSourceCap)
	got, truncated := clipboardSequence(text, false, false)
	if truncated {
		t.Fatal("a payload exactly at the budget was truncated")
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), "\a")
	if len(payload) > clipboardPayloadCap {
		t.Fatalf("payload is %d bytes, want <= %d", len(payload), clipboardPayloadCap)
	}
}

func TestUnit_CopyToClipboardReadsEnvAtCallTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sink: %v", err)
	}
	defer f.Close()
	e := &ANSI{out: f}

	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	if _, err := e.CopyToClipboard("hi"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	if _, err := e.CopyToClipboard("hi"); err != nil {
		t.Fatalf("copy under tmux: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	plain, _ := clipboardSequence("hi", false, false)
	wrapped, _ := clipboardSequence("hi", true, false)
	if want := plain + wrapped; string(written) != want {
		t.Fatalf("clipboard writes\n got %q\nwant %q", written, want)
	}
}
