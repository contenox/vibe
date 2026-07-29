//go:build llamanode

package llamasession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/modeld/llama"
	"github.com/contenox/contenox/internal/modeld/llama/llamacppshim"
)

func TestSystem_LlamaSessionTiny_PopulatesManifestTokenRanges(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	sess, err := New(modelPath, llama.Config{
		NumCtx:     128,
		NumBatch:   16,
		NumThreads: 1,
		DisableBOS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	stable := "system\n"
	suffix := "user\n"
	manifest := tinyManifest(stable, suffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if prefix.PrefixTokens == 0 || prefix.StableTokenHash == "" {
		t.Fatalf("prefix status missing token data: %+v", prefix)
	}
	suffixStatus, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	if suffixStatus.SuffixTokens == 0 || suffixStatus.ResidentTokens <= suffixStatus.PrefixTokens {
		t.Fatalf("suffix status missing token data: %+v", suffixStatus)
	}

	report := sess.ExplainContext()
	if report.ManifestDigest == "" || report.Manifest.StableTokenHash == "" || report.Manifest.VolatileTokenHash == "" {
		t.Fatalf("context report missing manifest token hashes: %+v", report)
	}
	if len(report.Manifest.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(report.Manifest.Segments))
	}
	for _, seg := range report.Manifest.Segments {
		if seg.TokenHash == "" || seg.TokenEnd <= seg.TokenStart {
			t.Fatalf("segment token range/hash not populated: %+v", seg)
		}
	}
	if got := report.Manifest.Segments[0].TokenEnd; got != report.PrefixTokens {
		t.Fatalf("stable token end = %d, want prefix tokens %d", got, report.PrefixTokens)
	}
	if got := report.Manifest.Segments[1].TokenStart; got != report.PrefixTokens {
		t.Fatalf("volatile token start = %d, want prefix tokens %d", got, report.PrefixTokens)
	}
}

func TestSystem_LlamaSessionTiny_WarmSuffixEqualsColdOneToken(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	cfg := llama.Config{
		NumCtx:     128,
		NumBatch:   16,
		NumThreads: 1,
		DisableBOS: true,
	}
	stable := "system\n"
	suffix := "user\n"

	cold, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cold.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	coldText, _, err := tinyTurn(ctx, cold, stable, suffix)
	if err != nil {
		t.Fatal(err)
	}
	if coldText == "" {
		t.Skip("tiny model produced no visible token for the cold continuation")
	}

	warm, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer warm.Close()
	if _, _, err := tinyTurn(ctx, warm, stable, "old\n"); err != nil {
		t.Fatal(err)
	}
	warmText, warmPrefix, err := tinyTurn(ctx, warm, stable, suffix)
	if err != nil {
		t.Fatal(err)
	}
	if warmPrefix.ReusedTokens != warmPrefix.PrefixTokens {
		t.Fatalf("warm turn reused %d prefix tokens, want full prefix %d", warmPrefix.ReusedTokens, warmPrefix.PrefixTokens)
	}
	if warmText != coldText {
		t.Fatalf("warm continuation %q != cold continuation %q", warmText, coldText)
	}
}

func TestSystem_LlamaSessionTiny_SnapshotRestoreOneToken(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)

	cfg := llama.Config{NumCtx: 128, NumBatch: 16, NumThreads: 1, DisableBOS: true}
	stable := "system\n"
	suffix := "user\n"
	manifest := tinyManifest(stable, suffix)

	original, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := original.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	if _, err := original.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
		t.Fatal(err)
	}
	snap, err := original.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeOne(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		t.Skip("tiny model produced no visible token for the original continuation")
	}

	restored, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, err := decodeOne(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("restored continuation %q != original continuation %q", got, want)
	}
}

func TestSystem_LlamaSessionTiny_ToolsRenderThroughSession(t *testing.T) {
	modelPath := os.Getenv("CONTENOX_LLAMA_TINY_GGUF")
	requireTinyGGUF(t, modelPath)
	probe, err := llamacppshim.ProbeChatTemplate(modelPath)
	if err != nil {
		t.Fatalf("probe chat template: %v", err)
	}
	if !probe.SupportsToolCalls {
		t.Skipf("tiny model chat template %q does not support native tool rendering", probe.FormatName)
	}

	cfg := llama.Config{NumCtx: 512, NumBatch: 16, NumThreads: 1, DisableBOS: true}
	stable := "system\nYou are a helpful assistant.\n"
	manifest := tinyManifest(stable, "")
	tools := `[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bare, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	noTools, err := bare.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}

	withTools, err := New(modelPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer withTools.Close()
	tooled, err := withTools.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}

	if tooled.PrefixTokens <= noTools.PrefixTokens {
		t.Fatalf("tools did not reach the renderer: with-tools tokens=%d, no-tools tokens=%d",
			tooled.PrefixTokens, noTools.PrefixTokens)
	}
	t.Logf("prefix tokens: no-tools=%d with-tools=%d (+%d for the rendered tool block)",
		noTools.PrefixTokens, tooled.PrefixTokens, tooled.PrefixTokens-noTools.PrefixTokens)
}

func requireTinyGGUF(t *testing.T, modelPath string) {
	t.Helper()
	if modelPath == "" {
		t.Skip("set CONTENOX_LLAMA_TINY_GGUF to a very small GGUF to run this test")
	}
	info, err := os.Stat(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	const maxTinySize = 512 << 20
	if info.Size() > maxTinySize {
		t.Skipf("refusing non-tiny GGUF %s: size=%d max=%d", modelPath, info.Size(), maxTinySize)
	}
}

func tinyTurn(ctx context.Context, sess llama.Session, stable, suffix string) (string, llama.PrefixStatus, error) {
	manifest := tinyManifest(stable, suffix)
	prefix, err := sess.EnsurePrefix(ctx, llama.PrefixInput{Text: stable, Manifest: manifest})
	if err != nil {
		return "", prefix, err
	}
	if _, err := sess.PrefillSuffix(ctx, llama.SuffixInput{Text: suffix, Manifest: manifest}); err != nil {
		return "", prefix, err
	}
	temp := 0.0
	seed := 7
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: 1, Temperature: &temp, Seed: &seed})
	if err != nil {
		return "", prefix, err
	}
	var out string
	for chunk := range chunks {
		if chunk.Error != nil {
			return "", prefix, chunk.Error
		}
		out += chunk.Text
	}
	return out, prefix, nil
}

func decodeOne(ctx context.Context, sess llama.Session) (string, error) {
	temp := 0.0
	seed := 7
	chunks, err := sess.Decode(ctx, llama.DecodeConfig{MaxTokens: 1, Temperature: &temp, Seed: &seed})
	if err != nil {
		return "", err
	}
	var out string
	for chunk := range chunks {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		out += chunk.Text
	}
	return out, nil
}

func tinyManifest(stable, suffix string) llama.ContextManifest {
	return llama.ContextManifest{
		ProfileID:            "tiny-test",
		Backend:              "llamacpp",
		BackendVersion:       "test",
		ModelDigest:          "tiny",
		PromptFormat:         "chatml",
		PromptTemplateDigest: "test-template",
		RuntimeDigest:        "test-runtime",
		AddBOS:               false,
		StableBytes:          len(stable),
		TotalBytes:           len(stable) + len(suffix),
		StableByteHash:       shaHex(stable),
		Segments: []llama.ManifestSegment{
			{Kind: "system", Stable: true, ByteStart: 0, ByteEnd: len(stable), ByteHash: shaHex(stable)},
			{Kind: "user", Stable: false, ByteStart: len(stable), ByteEnd: len(stable) + len(suffix), ByteHash: shaHex(suffix)},
		},
	}
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
