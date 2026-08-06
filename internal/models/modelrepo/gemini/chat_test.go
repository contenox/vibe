package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/libtracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestChatClient(srv *httptest.Server) *GeminiChatClient {
	return &GeminiChatClient{
		geminiClient: geminiClient{
			apiKey:     "test-key",
			modelName:  "gemini-test",
			baseURL:    srv.URL,
			httpClient: srv.Client(),
			tracker:    libtracker.NoopTracker{},
		},
	}
}

func TestUnit_GeminiChat_ThinkingOnlyResponseIsNotAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"reasoning about the task","thought":true}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	res, err := newTestChatClient(srv).Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})

	require.NoError(t, err, "Gemini returning only a thinking part (no final text, no tool call) must not be a hard 'empty content' error: that error is classified retryable, exhausts retries, fails the task, cascades acp_chat->recovery_chat->summarise_failure, and surfaces as a silent max_turn_requests with nothing rendered")
	assert.Equal(t, "", res.Message.Content)
	assert.Equal(t, "reasoning about the task", res.Message.Thinking)
	assert.Empty(t, res.ToolCalls)
}

func TestUnit_GeminiChat_EmptyPartsAreToleratedLikeOllama(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"thoughtSignature":"sig-only"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	res, err := newTestChatClient(srv).Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})

	require.NoError(t, err, "a signature-only / empty turn on a normal finish reason must be tolerated as a degenerate end-of-turn signal, matching the Ollama handler, instead of cascading into a silent dead turn")
	assert.Equal(t, "", res.Message.Content)
	assert.Empty(t, res.ToolCalls)
}

func TestUnit_GeminiChat_NormalTextStillWorks(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello there"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	res, err := newTestChatClient(srv).Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})

	require.NoError(t, err)
	assert.Equal(t, "hello there", res.Message.Content)
}

func TestUnit_GeminiChat_ClampsMaxOutputTokens(t *testing.T) {
	t.Parallel()

	var got geminiGenerateContentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &got))
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	client := newTestChatClient(srv)
	client.maxOutputTokens = 128
	_, err := client.Chat(context.Background(),
		[]modelrepo.Message{{Role: "user", Content: "hi"}},
		modelrepo.WithMaxTokens(999),
	)
	require.NoError(t, err)
	require.NotNil(t, got.GenerationConfig)
	require.NotNil(t, got.GenerationConfig.MaxOutputTokens)
	assert.Equal(t, 128, *got.GenerationConfig.MaxOutputTokens)
}

func TestUnit_GeminiChat_BlockedPromptStillErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`)
	}))
	defer srv.Close()

	_, err := newTestChatClient(srv).Chat(context.Background(), []modelrepo.Message{{Role: "user", Content: "hi"}})

	require.Error(t, err, "a genuinely blocked prompt (no candidates) must still surface an error, not be silently tolerated")
}

func TestUnit_BuildGeminiRequest_MapsThinkingConfig(t *testing.T) {
	t.Parallel()
	msgs := []modelrepo.Message{{Role: "user", Content: "hi"}}

	req, err := buildGeminiRequest("gemini-2.5-pro", msgs, []modelrepo.ChatArgument{modelrepo.WithThink("medium")})
	require.NoError(t, err)
	require.NotNil(t, req.GenerationConfig.ThinkingConfig)
	require.NotNil(t, req.GenerationConfig.ThinkingConfig.ThinkingBudget)
	assert.Equal(t, 8192, *req.GenerationConfig.ThinkingConfig.ThinkingBudget)
	assert.Equal(t, "", req.GenerationConfig.ThinkingConfig.ThinkingLevel)

	req, err = buildGeminiRequest("gemini-3-flash", msgs, []modelrepo.ChatArgument{modelrepo.WithThink("off")})
	require.NoError(t, err)
	require.NotNil(t, req.GenerationConfig.ThinkingConfig)
	require.Nil(t, req.GenerationConfig.ThinkingConfig.ThinkingBudget)
	assert.Equal(t, "minimal", req.GenerationConfig.ThinkingConfig.ThinkingLevel)

	req, err = buildGeminiRequest("gemini-3-pro", msgs, []modelrepo.ChatArgument{modelrepo.WithThink("auto")})
	require.NoError(t, err)
	require.Nil(t, req.GenerationConfig.ThinkingConfig)

	req, err = buildGeminiRequest("gemini-2.5-pro", msgs, []modelrepo.ChatArgument{modelrepo.WithThink("medium")}, false)
	require.NoError(t, err)
	require.Nil(t, req.GenerationConfig.ThinkingConfig, "provider with CanThink=false must omit Gemini thinking config")
}

func TestUnit_BuildGeminiRequest_RejectsEmptyContents(t *testing.T) {
	t.Parallel()

	_, err := buildGeminiRequest("gemini-3.1-pro-preview",
		[]modelrepo.Message{{Role: "system", Content: "system only"}},
		nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to send empty contents")
	require.Contains(t, err.Error(), "provide at least one non-empty")

	_, err = buildGeminiRequest("gemini-3.1-pro-preview",
		[]modelrepo.Message{{Role: "user", Content: ""}},
		nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to send empty contents")
}

func TestUnit_BuildGeminiRequest_WrapsSchemaLikeToolResultAsText(t *testing.T) {
	t.Parallel()

	const schemaResult = `{"$defs":{"LogoutCapabilities":{"type":"object"}},"properties":{"logout":{"$ref":"#/$defs/LogoutCapabilities"}}}`
	msgs := []modelrepo.Message{
		{
			Role: "assistant",
			ToolCalls: []modelrepo.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "webtools.web_get", Arguments: `{"url":"https://example.test/schema.json"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Content: schemaResult},
	}

	req, err := buildGeminiRequest("gemini-3.1-pro-preview", msgs, nil)
	require.NoError(t, err)
	require.Len(t, req.Contents, 2)
	resp := req.Contents[1].Parts[0].FunctionResponse.Response
	require.Equal(t, schemaResult, resp["content"])
	require.NotContains(t, resp, "$defs")
}

func TestUnit_BuildGeminiRequest_ImageInputAddsInlineDataPart(t *testing.T) {
	t.Parallel()

	imgBytes := []byte("fake-png-bytes")
	wantB64 := base64.StdEncoding.EncodeToString(imgBytes)

	msgs := []modelrepo.Message{
		{
			Role:    "user",
			Content: "what is in this image?",
			Images:  []modelrepo.ImagePart{{Data: imgBytes, MimeType: "image/png"}},
		},
	}

	req, err := buildGeminiRequest("gemini-2.5-pro", msgs, nil)
	require.NoError(t, err)

	require.Len(t, req.Contents, 1)
	parts := req.Contents[0].Parts
	require.Len(t, parts, 2)
	assert.Equal(t, "what is in this image?", parts[0].Text)
	require.NotNil(t, parts[1].InlineData)
	assert.Equal(t, "image/png", parts[1].InlineData.MimeType)
	assert.Equal(t, imgBytes, parts[1].InlineData.Data)

	// Wire shape: base64 payload and mime type round-trip in the marshaled JSON.
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	js := string(raw)
	assert.Contains(t, js, `"inlineData":{`)
	assert.Contains(t, js, `"mimeType":"image/png"`)
	assert.Contains(t, js, `"data":"`+wantB64+`"`)

	// A text-only message keeps its prior single text-part shape (no inlineData).
	textReq, err := buildGeminiRequest("gemini-2.5-pro", []modelrepo.Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	require.Len(t, textReq.Contents, 1)
	require.Len(t, textReq.Contents[0].Parts, 1)
	assert.Equal(t, "hi", textReq.Contents[0].Parts[0].Text)
	assert.Nil(t, textReq.Contents[0].Parts[0].InlineData)
	textRaw, err := json.Marshal(textReq)
	require.NoError(t, err)
	assert.NotContains(t, string(textRaw), "inlineData")
}

func TestUnit_BuildGeminiRequest_KeepsNormalObjectToolResultStructured(t *testing.T) {
	t.Parallel()

	msgs := []modelrepo.Message{
		{
			Role: "assistant",
			ToolCalls: []modelrepo.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "webtools.web_get", Arguments: `{"url":"https://example.test/data.json"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Content: `{"status":"ok"}`},
	}

	req, err := buildGeminiRequest("gemini-3.1-pro-preview", msgs, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", req.Contents[1].Parts[0].FunctionResponse.Response["status"])
}
