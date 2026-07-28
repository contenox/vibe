package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// Every request carries store:false, and reasoning is parsed from the "reasoning" output item, never the top-level config echo.
func TestUnit_OpenAIChatClient_ResponsesReasoningAndStore(t *testing.T) {
	t.Parallel()

	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.Header().Set("Content-Type", "application/json")
		// The decoy top-level "reasoning" echo must be ignored by the parser.
		_, _ = w.Write([]byte(`{
			"output": [
				{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"chain of thought"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}
			],
			"reasoning": {"effort":"high","summary":"auto"},
			"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	client := &OpenAIChatClient{openAIClient: openAIClient{
		baseURL:       srv.URL,
		apiKey:        "k",
		httpClient:    srv.Client(),
		modelName:     "gpt-5",
		supportsThink: true,
		tracker:       libtracker.NoopTracker{},
	}}

	res, err := client.Chat(context.Background(),
		[]modelrepo.Message{{Role: "user", Content: "hi"}},
		modelrepo.WithThink("high"),
	)
	require.NoError(t, err)
	require.Equal(t, "final answer", res.Message.Content)
	require.Equal(t, "chain of thought", res.Message.Thinking,
		"reasoning must come from the reasoning output item, not the config echo")

	require.Equal(t, false, gotReq["store"], "store:false must be sent on every Responses request")
	reasoning, ok := gotReq["reasoning"].(map[string]any)
	require.True(t, ok, "request must carry a reasoning config")
	require.Equal(t, "high", reasoning["effort"])
	require.Equal(t, "auto", reasoning["summary"], "summaries must be requested explicitly")
}
