package chatapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/openaichatservice"
	"github.com/contenox/vibe/taskengine"
)

const streamSpeed = 50 * time.Millisecond

// SetTaskChainRequest defines the expected structure for configuring the task chain
type SetTaskChainRequest struct {
	// The ID of the task chain to use for OpenAI-compatible chat completions
	TaskChainID string `json:"taskChainID" example:"openai-compatible-chain"`
}

func AddChatRoutes(mux *http.ServeMux, chatService openaichatservice.Service) {
	h := &handler{service: chatService}

	// OpenAI-compatible endpoints
	mux.HandleFunc("POST /openai/{chainID}/v1/chat/completions", h.openAIChatCompletions)
}

type handler struct {
	service openaichatservice.Service
}

type OpenAIChatResponse struct {
	ID                string                                `json:"id" example:"chat_123"`
	Object            string                                `json:"object" example:"chat.completion"`
	Created           int64                                 `json:"created" example:"1690000000"`
	Model             string                                `json:"model" example:"mistral:instruct"`
	Choices           []taskengine.OpenAIChatResponseChoice `json:"choices" openapi_include_type:"taskengine.OpenAIChatResponseChoice"`
	Usage             taskengine.OpenAITokenUsage           `json:"usage" openapi_include_type:"taskengine.OpenAITokenUsage"`
	SystemFingerprint string                                `json:"system_fingerprint,omitempty" example:"system_456"`
	StackTrace        []taskengine.CapturedStateUnit        `json:"stackTrace,omitempty"`
}

// Processes chat requests using the configured task chain.
//
// This endpoint provides OpenAI-compatible chat completions by executing
// the configured task chain with the provided request data.
// The task chain must be configured first using the /chat/taskchain endpoint.
//
// --- SSE Streaming ---
// When 'stream: true' is set in the request body, the endpoint streams the response
// using Server-Sent Events (SSE) in the OpenAI-compatible format.
//
// Clients should concatenate the 'content' from the 'delta' object in each 'data' event
// to reconstruct the full message. The stream is terminated by a 'data: [DONE]' message.
//
// Example event stream:
// data: {"id":"chat_123","object":"chat.completion.chunk","created":1690000000,"model":"mistral:instruct","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
//
// data: {"id":"chat_123","object":"chat.completion.chunk","created":1690000000,"model":"mistral:instruct","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}
//
// data: [DONE]
func (h *handler) openAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chainID := apiframework.GetPathParam(r, "chainID", "The ID of the task chain to use.")
	req, err := apiframework.Decode[taskengine.OpenAIChatRequest](r) // @request taskengine.OpenAIChatRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	// Check the 'stream' flag from the request body.
	if req.Stream {
		h.handleStream(w, r, chainID, req)
		return
	}

	// Handle non-streaming requests as before.
	addTraces := apiframework.GetQueryParam(r, "stackTrace", "false", "If provided the stacktraces will be added to the response.")
	chatResp, traces, err := h.service.OpenAIChatCompletions(ctx, chainID, req)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	resp := OpenAIChatResponse{
		ID:                chatResp.ID,
		Object:            chatResp.Object,
		Created:           chatResp.Created,
		Model:             chatResp.Model,
		Choices:           chatResp.Choices,
		Usage:             chatResp.Usage,
		SystemFingerprint: chatResp.SystemFingerprint,
		StackTrace:        traces,
	}
	if addTraces != "true" && addTraces != "True" {
		resp.StackTrace = nil
	}
	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response chatapi.OpenAIChatResponse
}

func (h *handler) handleStream(w http.ResponseWriter, r *http.Request, chainID string, req taskengine.OpenAIChatRequest) {
	ctx := r.Context()
	streamChan, err := h.service.OpenAIChatCompletionsStream(ctx, chainID, req, streamSpeed)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	// Set headers for Server-Sent Events (SSE).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = apiframework.Error(w, r, fmt.Errorf("streaming unsupported"), apiframework.CreateOperation)
		return
	}

	for chunk := range streamChan {
		jsonData, err := json.Marshal(chunk)
		if err != nil {
			// TODO: deal with the error.
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type chainIDResponse struct {
	// The ID of the Task-Chain used as default for Open-AI chat/completions.
	ChainID string `json:"taskChainID" example:"openai-compatible-chain"`
}
