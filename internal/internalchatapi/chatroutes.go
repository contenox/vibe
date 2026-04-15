package internalchatapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/chatsessionmodes"
	"github.com/contenox/contenox/taskengine"
)

func AddChatRoutes(
	mux *http.ServeMux,
	chat chatsessionmodes.HTTPChat,
	auth middleware.AuthZReader,
) {
	h := &chatManagerHandler{
		chatSvc: chat,
		auth:    auth,
	}

	mux.HandleFunc("POST /chats", h.createChat)
	mux.HandleFunc("POST /chats/{id}/chat", h.chat)
	mux.HandleFunc("GET /chats/{id}", h.history)
	mux.HandleFunc("GET /chats", h.listChats)
}

type chatManagerHandler struct {
	chatSvc chatsessionmodes.HTTPChat
	auth    middleware.AuthZReader
}

type newChatInstanceRequest struct {
	Model string `json:"model"`
}

// Creates a new chat instance for the specified subject.
//
// Returns the unique identifier for the new chat session.
func (h *chatManagerHandler) createChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	req, err := apiframework.Decode[newChatInstanceRequest](r) // @request internalchatapi.newChatInstanceRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	chatID, startedAt, err := h.chatSvc.CreateChatSession(ctx, identity)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	resp := chatSession{
		ID:        chatID,
		StartedAt: startedAt,
		Model:     req.Model,
	}
	_ = apiframework.Encode(w, r, http.StatusCreated, resp) // @response internalchatapi.chatSession
}

type chatSession struct {
	ID          string       `json:"id"`
	StartedAt   time.Time    `json:"startedAt"`
	Model       string       `json:"model"`
	LastMessage *chatMessage `json:"lastMessage,omitempty"`
}

type chatMessage struct {
	ID         string                `json:"id"`
	Role       string                `json:"role"`
	Content    string                `json:"content"`
	SentAt     time.Time             `json:"sentAt"`
	IsUser     bool                  `json:"isUser"`
	IsLatest   bool                  `json:"isLatest"`
	CallTools  []taskengine.ToolCall `json:"callTools,omitempty" openapi_include_type:"taskengine.ToolCall"`
	ToolCallID string                `json:"toolCallId,omitempty"`
}

type chatRequest struct {
	Message string `json:"message"`
	// Mode selects default task chain when chainId query is omitted: chat, prompt, plan, build.
	// build compiles the active plan and runs the compiled chain; chainId must be the executor chain ref; message may be empty.
	Mode string `json:"mode,omitempty"`
	// Context carries structured artifacts merged as system messages before this user turn.
	Context *chatContextPayload `json:"context,omitempty"`
}

type chatContextPayload struct {
	Artifacts []contextArtifact `json:"artifacts,omitempty"`
}

type contextArtifact struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type chatResponse struct {
	Response         string                         `json:"response"`
	State            []taskengine.CapturedStateUnit `json:"state" openapi_include_type:"taskengine.CapturedStateUnit"`
	InputTokenCount  int                            `json:"inputTokenCount"`
	OutputTokenCount int                            `json:"outputTokenCount"`
	Error            string                         `json:"error,omitempty"`
}

func toTurnContext(c *chatContextPayload) *chatsessionmodes.ContextPayload {
	if c == nil {
		return nil
	}
	out := &chatsessionmodes.ContextPayload{Artifacts: make([]chatsessionmodes.ContextArtifact, len(c.Artifacts))}
	for i := range c.Artifacts {
		out.Artifacts[i] = chatsessionmodes.ContextArtifact{Kind: c.Artifacts[i].Kind, Payload: c.Artifacts[i].Payload}
	}
	return out
}

// Sends a message to a chat session and gets AI response.
//
// Supports multiple AI models and providers with token counting and state capture.
func (h *chatManagerHandler) chat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	idStr := apiframework.GetPathParam(r, "id", "The unique identifier of the chat session.") // @param id string
	if idStr == "" {
		apiframework.Error(w, r, fmt.Errorf("chat ID is required: %w", apiframework.ErrBadPathValue), apiframework.CreateOperation)
		return
	}

	chainIDQuery := apiframework.GetQueryParam(r, "chainId", "", "The ID of the taskchain to be used to compute the response. When omitted, mode in the body selects a default chain.") // @param chainId string

	req, err := apiframework.Decode[chatRequest](r) // @request internalchatapi.chatRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	if req.Message == "" && !strings.EqualFold(strings.TrimSpace(req.Mode), "build") {
		_ = apiframework.Error(w, r, apiframework.BadRequest("Message body is required."), apiframework.CreateOperation)
		return
	}

	model := apiframework.GetQueryParam(r, "model", "", "Optional model override used when the task chain references {{var:model}}.")             // @param model string
	provider := apiframework.GetQueryParam(r, "provider", "", "Optional provider override used when the task chain references {{var:provider}}.") // @param provider string

	turnIn := chatsessionmodes.TurnInput{
		SessionID:        idStr,
		Message:          req.Message,
		ExplicitChainRef: chainIDQuery,
		Mode:             req.Mode,
		Context:          toTurnContext(req.Context),
		Model:            strings.TrimSpace(model),
		Provider:         strings.TrimSpace(provider),
	}

	result, err := h.chatSvc.SendTurn(ctx, turnIn)
	if err != nil {
		if isBadRequestChatErr(err) {
			_ = apiframework.Error(w, r, apiframework.BadRequest(err.Error()), apiframework.CreateOperation)
			return
		}
		if isChainResolveErr(err) {
			_ = apiframework.Error(w, r, apiframework.BadRequest(err.Error()), apiframework.CreateOperation)
			return
		}
		if errors.Is(err, chatsessionmodes.ErrMissingModelProvider) {
			_ = apiframework.Error(w, r, apiframework.BadRequest(
				"Model and provider are required for task chains that use {{var:model}} / {{var:provider}}. "+
					"Set them with `contenox config set default-model <name>` and `contenox config set default-provider <type>` "+
					"(for example ollama), or pass ?model=...&provider=... on the chat request.",
			), apiframework.CreateOperation)
			return
		}
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	resp := chatResponse{
		Response:         result.Response,
		State:            result.State,
		InputTokenCount:  result.InputTokenCount,
		OutputTokenCount: result.OutputTokenCount,
	}
	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response internalchatapi.chatResponse
}

func isBadRequestChatErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "context.artifacts") ||
		strings.Contains(s, "injected context exceeds") ||
		strings.Contains(s, "internal error: empty thread") ||
		strings.Contains(s, "internal error: expected last message") ||
		strings.Contains(s, "no active plan") ||
		strings.Contains(s, "plancompile:")
}

func isChainResolveErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "chainId query parameter") ||
		strings.Contains(s, "unknown chat mode") ||
		strings.Contains(s, "required for build mode")
}

// Retrieves the complete chat history for a session.
//
// Returns all messages and interactions in chronological order.
func (h *chatManagerHandler) history(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	idStr := apiframework.GetPathParam(r, "id", "The unique identifier of the chat session.") // @param id string
	if idStr == "" {
		apiframework.Error(w, r, fmt.Errorf("chat ID is required: %w", apiframework.ErrBadPathValue), apiframework.GetOperation)
		return
	}

	history, err := h.chatSvc.ListChatMessages(ctx, idStr)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	resp := make([]chatMessage, 0, len(history))
	for _, msg := range history {
		resp = append(resp, chatMessage{
			ID:         msg.ID,
			Role:       msg.Role,
			Content:    msg.Content,
			SentAt:     msg.Timestamp,
			IsUser:     msg.Role == "user",
			CallTools:  msg.CallTools,
			ToolCallID: msg.ToolCallID,
		})
	}
	if len(resp) > 0 {
		resp[len(resp)-1].IsLatest = true
	}
	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response []internalchatapi.chatMessage
}

// Lists all available chat sessions.
//
// Returns basic information about each chat session in the system.
func (h *chatManagerHandler) listChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity, err := h.auth.GetIdentity(ctx)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	sessions, err := h.chatSvc.ListChatSessions(ctx, identity)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	resp := make([]chatSession, 0, len(sessions))
	for _, session := range sessions {
		item := chatSession{ID: session.ID, StartedAt: session.StartedAt}
		if session.LastMessage != nil {
			item.LastMessage = &chatMessage{
				ID:      session.LastMessage.ID,
				Role:    session.LastMessage.Role,
				Content: session.LastMessage.Content,
				SentAt:  session.LastMessage.SentAt,
				IsUser:  session.LastMessage.IsUser,
			}
		}
		resp = append(resp, item)
	}

	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response []internalchatapi.chatSession
}
