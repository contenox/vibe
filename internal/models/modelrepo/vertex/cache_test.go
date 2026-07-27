package vertex

import (
	"encoding/json"
	"testing"
)

func TestUnit_VertexUsage_CachedContentTokenCount(t *testing.T) {
	// Vertex mirrors Gemini: promptTokenCount is already the TOTAL prompt
	// count; cachedContentTokenCount breaks out context-cache hits and must
	// NOT be added on top.
	body := `{"promptTokenCount":4200,"candidatesTokenCount":15,"totalTokenCount":4215,"cachedContentTokenCount":4096}`
	var meta vertexUsageMetadata
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatal(err)
	}
	u := meta.neutralUsage()
	if u.PromptTokens != 4200 || u.CacheReadTokens != 4096 || u.CacheWriteTokens != 0 {
		t.Fatalf("vertex usage extraction wrong: %+v", u)
	}
	if u.CompletionTokens != 15 || u.TotalTokens != 4215 {
		t.Fatalf("completion/total wrong: %+v", u)
	}

	var nilMeta *vertexUsageMetadata
	if nilMeta.neutralUsage() != nil {
		t.Fatal("nil usageMetadata must stay nil")
	}
}
