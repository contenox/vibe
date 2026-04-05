package internalchatapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	"github.com/contenox/contenox/chatservice"
	"github.com/contenox/contenox/execservice"
	"github.com/contenox/contenox/internal/clikv"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/messagestore"
	"github.com/contenox/contenox/runtimetypes"
	"github.com/contenox/contenox/taskchainservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/google/uuid"
)

func AddChatRoutes(
	mux *http.ServeMux,
	db libdbexec.DBManager,
	taskService execservice.TasksEnvService,
	taskChainService taskchainservice.Service,
	auth middleware.AuthZReader,
) {
	h := &chatManagerHandler{
		db:           db,
		taskService:  taskService,
		chainService: taskChainService,
		auth:         auth,
		chatManager:  chatservice.NewManager(nil),
	}

	mux.HandleFunc("POST /chats", h.createChat)
	mux.HandleFunc("POST /chats/{id}/chat", h.chat)
	mux.HandleFunc("GET /chats/{id}", h.history)
	mux.HandleFunc("GET /chats", h.listChats)
}

type chatManagerHandler struct {
	db           libdbexec.DBManager
	taskService  execservice.TasksEnvService
	chainService taskchainservice.Service
	auth         middleware.AuthZReader
	chatManager  *chatservice.Manager
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

	chatID := uuid.NewString()
	if err := messagestore.New(h.db.WithoutTransaction()).CreateMessageIndex(ctx, chatID, identity); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	resp := chatSession{
		ID:        chatID,
		StartedAt: time.Now().UTC(),
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
	ID       string    `json:"id"`
	Role     string    `json:"role"`
	Content  string    `json:"content"`
	SentAt   time.Time `json:"sentAt"`
	IsUser   bool      `json:"isUser"`
	IsLatest bool      `json:"isLatest"`
}

type chatRequest struct {
	Message string `json:"message"`
}

type chatResponse struct {
	Response         string                         `json:"response"`
	State            []taskengine.CapturedStateUnit `json:"state"`
	InputTokenCount  int                            `json:"inputTokenCount"`
	OutputTokenCount int                            `json:"outputTokenCount"`
	Error            string                         `json:"error,omitempty"`
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

	chainID := apiframework.GetQueryParam(r, "chainId", "", "The ID of the taskchain to be used to compute the response.") // @param chainId string

	req, err := apiframework.Decode[chatRequest](r) // @request internalchatapi.chatRequest
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}

	if req.Message == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("Message body is required."), apiframework.CreateOperation)
		return
	}

	tx := h.db.WithoutTransaction()
	messages, err := h.chatManager.ListMessages(ctx, tx, idStr)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	messages, err = h.chatManager.AppendMessage(ctx, messages, time.Now().UTC(), req.Message, "user")
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	chain, err := h.chainService.Get(ctx, chainID)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	rtStore := runtimetypes.New(h.db.WithoutTransaction())
	model := strings.TrimSpace(apiframework.GetQueryParam(r, "model", "", "Optional model override used when the task chain references {{var:model}}."))
	provider := strings.TrimSpace(apiframework.GetQueryParam(r, "provider", "", "Optional provider override used when the task chain references {{var:provider}}."))
	if model == "" {
		model = clikv.Read(ctx, rtStore, "default-model")
	}
	if provider == "" {
		provider = clikv.Read(ctx, rtStore, "default-provider")
	}
	if model == "" || provider == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest(
			"Model and provider are required for task chains that use {{var:model}} / {{var:provider}}. "+
				"Set them with `contenox config set default-model <name>` and `contenox config set default-provider <type>` "+
				"(for example ollama), or pass ?model=...&provider=... on the chat request.",
		), apiframework.CreateOperation)
		return
	}
	templateVars := map[string]string{
		"model":    model,
		"provider": provider,
	}
	if reqID, ok := ctx.Value(libtracker.ContextKeyRequestID).(string); ok && reqID != "" {
		templateVars["request_id"] = reqID
	}
	execCtx := taskengine.WithTemplateVars(ctx, templateVars)

	result, dt, capturedStateUnits, err := h.taskService.Execute(execCtx, chain, taskengine.ChatHistory{Messages: messages}, taskengine.DataTypeChatHistory)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.CreateOperation)
		return
	}
	resp := chatResponse{
		State:            capturedStateUnits,
	}
	switch dt {
	case taskengine.DataTypeChatHistory:
		history, ok := result.(taskengine.ChatHistory)
		if !ok || len(history.Messages) == 0 {
			_ = apiframework.Error(w, r, apiframework.UnprocessableEntity(
				"The task chain did not return a valid chat history (missing or empty messages).",
			), apiframework.CreateOperation)
			return
		}
		last := history.Messages[len(history.Messages)-1]
		resp.Response = last.Content
		resp.InputTokenCount = history.InputTokens
		resp.OutputTokenCount = history.OutputTokens
		if perr := h.chatManager.PersistDiff(ctx, tx, idStr, history.Messages); perr != nil {
			_ = apiframework.Error(w, r, perr, apiframework.UpdateOperation)
			return
		}
	case taskengine.DataTypeOpenAIChatResponse:
		openaiResp, ok := result.(taskengine.OpenAIChatResponse)
		if !ok || len(openaiResp.Choices) == 0 || openaiResp.Choices[0].Message.Content == nil {
			_ = apiframework.Error(w, r, apiframework.UnprocessableEntity(
				"The task chain returned an OpenAI-style response without assistant text.",
			), apiframework.CreateOperation)
			return
		}
		resp.Response = *openaiResp.Choices[0].Message.Content
		resp.InputTokenCount = openaiResp.Usage.PromptTokens
		resp.OutputTokenCount = openaiResp.Usage.CompletionTokens
		messages = append(messages, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "assistant",
			Content:   resp.Response,
			Timestamp: time.Now().UTC(),
		})
		if perr := h.chatManager.PersistDiff(ctx, tx, idStr, messages); perr != nil {
			_ = apiframework.Error(w, r, perr, apiframework.UpdateOperation)
			return
		}
	case taskengine.DataTypeString, taskengine.DataTypeJSON, taskengine.DataTypeAny, taskengine.DataTypeNil,
		taskengine.DataTypeBool, taskengine.DataTypeInt, taskengine.DataTypeFloat,
		taskengine.DataTypeVector, taskengine.DataTypeSearchResults:
		resp.Response = formatChainResultForChat(result)
		messages = append(messages, taskengine.Message{
			ID:        uuid.NewString(),
			Role:      "assistant",
			Content:   resp.Response,
			Timestamp: time.Now().UTC(),
		})
		if perr := h.chatManager.PersistDiff(ctx, tx, idStr, messages); perr != nil {
			_ = apiframework.Error(w, r, perr, apiframework.UpdateOperation)
			return
		}
	default:
		_ = apiframework.Error(w, r, apiframework.UnprocessableEntity(
			fmt.Sprintf("This chat endpoint does not support task chain output type %q (chain %q).", dt.String(), chainID),
		), apiframework.CreateOperation)
		return
	}

	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response internalchatapi.chatResponse
}

// formatChainResultForChat turns arbitrary chain outputs into a single assistant message string.
func formatChainResultForChat(result any) string {
	if result == nil {
		return ""
	}
	switch v := result.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case json.RawMessage:
		return string(v)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
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

	history, err := h.chatManager.ListMessages(ctx, h.db.WithoutTransaction(), idStr)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}

	resp := make([]chatMessage, 0, len(history))
	for _, msg := range history {
		resp = append(resp, chatMessage{
			ID:      msg.ID,
			Role:    msg.Role,
			Content: msg.Content,
			SentAt:  msg.Timestamp,
			IsUser:  msg.Role == "user",
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

	store := messagestore.New(h.db.WithoutTransaction())
	sessions, err := store.ListAllSessions(ctx, identity)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.ListOperation)
		return
	}

	resp := make([]chatSession, 0, len(sessions))
	for _, session := range sessions {
		item := chatSession{ID: session.ID, StartedAt: time.Now().UTC()}
		last, lerr := store.LastMessage(ctx, session.ID)
		if lerr == nil && last != nil {
			var parsed taskengine.Message
			if jerr := json.Unmarshal(last.Payload, &parsed); jerr == nil {
				item.LastMessage = &chatMessage{
					ID:      parsed.ID,
					Role:    parsed.Role,
					Content: parsed.Content,
					SentAt:  last.AddedAt,
					IsUser:  parsed.Role == "user",
				}
				item.StartedAt = last.AddedAt
			}
		} else if lerr != nil && !errors.Is(lerr, messagestore.ErrNotFound) {
			_ = apiframework.Error(w, r, lerr, apiframework.ListOperation)
			return
		}
		resp = append(resp, item)
	}

	_ = apiframework.Encode(w, r, http.StatusOK, resp) // @response []internalchatapi.chatSession
}
