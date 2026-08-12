package localtools_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/stretchr/testify/require"
)

func tinyWAV(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(36+len(data))))
	b.WriteString("WAVEfmt ")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(16)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(1)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(1)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(8000)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(16000)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(2)))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint16(16)))
	b.WriteString("data")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, uint32(len(data))))
	b.Write(data)
	return b.Bytes()
}

func writeBytes(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, content, 0644))
	return p
}

func audioTools(dir string, fn localtools.AudioTranscriber) taskengine.ToolsRepo {
	return localtools.NewLocalFSToolsWith(dir, nil, nil, localtools.LocalFSToolsName, nil,
		localtools.WithAudioTranscriber(fn))
}

// read_file on a supported audio file returns the transcript as the tool
// result, with a notice naming file, type, and size.
func TestUnit_ReadFile_AudioReturnsTranscript(t *testing.T) {
	dir := t.TempDir()
	wav := tinyWAV(t, 64)
	writeBytes(t, dir, "note.wav", wav)

	var gotMime string
	var gotBytes int
	h := audioTools(dir, func(_ context.Context, data []byte, mimeType string) (string, error) {
		gotMime = mimeType
		gotBytes = len(data)
		return "hello from the stubbed audio model", nil
	})

	res, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "note.wav"})
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "hello from the stubbed audio model", "the transcript is the result")
	require.Contains(t, out, "note.wav", "the notice names the file")
	require.Contains(t, out, "audio/wav", "the notice names the detected type")
	require.Contains(t, out, "transcript", "the result says what it is")
	require.Equal(t, "audio/wav", gotMime, "the seam receives the canonical mime")
	require.Equal(t, len(wav), gotBytes, "the seam receives the whole file")
}

// Without a configured audio model, the refusal names the exact config key to set.
func TestUnit_ReadFile_AudioWithoutModelRefusesNamingConfigKey(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "note.wav", tinyWAV(t, 64))

	h := localtools.NewLocalFSTools(dir, nil) // no transcriber wired
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "note.wav"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "audio file")
	require.Contains(t, err.Error(), "default-audio-model", "the refusal names the config key")
	require.Contains(t, err.Error(), "no audio model is configured")
}

// Over the audio cap the refusal states the cap and the policy key that
// raises it, before any bytes are loaded or sent anywhere.
func TestUnit_ReadFile_AudioOverCapRefusesNamingCap(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "big.wav", tinyWAV(t, 512)) // 1068 bytes

	called := false
	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		called = true
		return "never", nil
	})
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_audio_bytes": "100",
	})
	_, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "big.wav"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "100-byte audio transcription cap", "the refusal states the cap")
	require.Contains(t, err.Error(), "_max_audio_bytes", "the refusal names the policy key")
	require.False(t, called, "nothing may reach the audio model over the cap")
}

// The audio cap, not _max_read_bytes, governs the audio path: a wav over the
// text-read budget still transcribes.
func TestUnit_ReadFile_AudioIgnoresTextReadCap(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "long.wav", tinyWAV(t, 4096)) // well formed, 8 KiB+

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return "transcribed", nil
	})
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_read_bytes": "1024", // far under the file size
	})
	res, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "long.wav"})
	require.NoError(t, err)
	require.Contains(t, res.(string), "transcribed")
}

// Audio outside the v1 set is refused with the detected type and supported
// set named, not a generic binary shrug.
func TestUnit_ReadFile_UnsupportedAudioFormatRefusesClassified(t *testing.T) {
	dir := t.TempDir()
	junk := bytes.Repeat([]byte{0x00, 0x91, 0x7F, 0xE2}, 64)
	writeBytes(t, dir, "voice.aiff", append([]byte("FORM\x00\x00\x01\x00AIFF"), junk...))

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return "never", nil
	})
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "voice.aiff"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "audio/aiff", "the refusal names what was detected")
	require.Contains(t, err.Error(), "wav, mp3, m4a, ogg, flac", "the refusal names the supported set")
}

// A binary file wearing an audio extension whose content matches no supported
// magic gets the audio-classified refusal, not the generic binary one.
func TestUnit_ReadFile_AudioExtensionWithForeignContentRefusesClassified(t *testing.T) {
	dir := t.TempDir()
	junk := bytes.Repeat([]byte{0x00, 0x91, 0x7F, 0xE2}, 64)
	writeBytes(t, dir, "song.aac", junk)

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return "never", nil
	})
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "song.aac"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "audio extension")
	require.Contains(t, err.Error(), "wav, mp3, m4a, ogg, flac")
}

// A text file with an audio extension is still just text: detection is by content.
func TestUnit_ReadFile_TextFileWithAudioExtensionStaysText(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "notes.mp3", "these are lyrics, not an mp3\n")

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return "never", nil
	})
	res, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "notes.mp3"})
	require.NoError(t, err)
	require.Equal(t, "these are lyrics, not an mp3\n", res.(string))
}

// A transcription failure surfaces as an error naming the file and the seam
// that failed, wrapped with the usual severity marker by Exec.
func TestUnit_ReadFile_AudioTranscriberErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "note.wav", tinyWAV(t, 64))

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return "", errors.New("backend unreachable")
	})
	_, err := execTool(t, context.Background(), h, "read_file", map[string]any{"path": "note.wav"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "transcribing")
	require.Contains(t, err.Error(), "note.wav")
	require.Contains(t, err.Error(), "backend unreachable")
}

// An oversized transcript is truncated with a notice naming the cap and the
// policy key — never silently.
func TestUnit_ReadFile_AudioTranscriptHonorsOutputCap(t *testing.T) {
	dir := t.TempDir()
	writeBytes(t, dir, "note.wav", tinyWAV(t, 64))

	h := audioTools(dir, func(context.Context, []byte, string) (string, error) {
		return strings.Repeat("word ", 2000), nil // ~10 KB transcript
	})
	ctx := taskengine.WithToolsArgs(context.Background(), localtools.LocalFSToolsName, map[string]string{
		"_max_output_bytes": "512",
	})
	res, err := execTool(t, ctx, h, "read_file", map[string]any{"path": "note.wav"})
	require.NoError(t, err)
	out := res.(string)
	require.Contains(t, out, "transcript truncated")
	require.Contains(t, out, "_max_output_bytes")
	require.Less(t, len(out), 1024, "the result stays near the cap")
}

// A transcript must not satisfy the read-before-write contract: overwriting
// the audio file still requires a real read.
func TestUnit_ReadFile_AudioTranscriptDoesNotUnlockOverwrite(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := libdb.NewSQLiteDBManager(ctx, dbPath, runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dir := t.TempDir()
	writeBytes(t, dir, "note.wav", tinyWAV(t, 64))
	h := localtools.NewLocalFSToolsWith(dir, db, nil, localtools.LocalFSToolsName, nil,
		localtools.WithAudioTranscriber(func(context.Context, []byte, string) (string, error) {
			return "transcript", nil
		}))
	ctxSession := context.WithValue(ctx, runtimetypes.SessionIDContextKey, "audio-session")

	res, err := execTool(t, ctxSession, h, "read_file", map[string]any{"path": "note.wav"})
	require.NoError(t, err)
	require.Contains(t, res.(string), "transcript")

	out, err := execTool(t, ctxSession, h, "write_file", map[string]any{"path": "note.wav", "content": "overwritten"})
	require.NoError(t, err, "the denial arrives as a soft tool result, not an error")
	refusal, ok := out.(localtools.FsRefusalResult)
	require.True(t, ok, "expected a read-before-write refusal, got %T: %v", out, out)
	require.True(t, refusal.Refused)
	require.Contains(t, refusal.Reason, "without reading it first")
}
