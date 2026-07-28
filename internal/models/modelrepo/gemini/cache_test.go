package gemini

import (
	"encoding/json"
	"testing"
)

func TestUnit_GeminiUsage_CachedContentTokenCount(t *testing.T) {
	// cachedContentTokenCount must not be added on top of promptTokenCount.
	body := `{"promptTokenCount":2100,"candidatesTokenCount":40,"totalTokenCount":2140,"cachedContentTokenCount":2048}`
	var meta geminiUsageMetadata
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatal(err)
	}
	u := meta.neutralUsage()
	if u.PromptTokens != 2100 || u.CacheReadTokens != 2048 || u.CacheWriteTokens != 0 {
		t.Fatalf("gemini usage extraction wrong: %+v", u)
	}
	if u.CompletionTokens != 40 || u.TotalTokens != 2140 {
		t.Fatalf("completion/total wrong: %+v", u)
	}

	var nilMeta *geminiUsageMetadata
	if nilMeta.neutralUsage() != nil {
		t.Fatal("nil usageMetadata must stay nil")
	}
}
