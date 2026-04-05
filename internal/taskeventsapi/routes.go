package taskeventsapi

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
	libbus "github.com/contenox/contenox/libbus"
	"github.com/contenox/contenox/taskengine"
)

func AddRoutes(mux *http.ServeMux, pubsub libbus.Messenger, auth middleware.AuthZReader) {
	h := &handler{
		pubsub: pubsub,
		auth:   auth,
	}
	mux.HandleFunc("GET /task-events", h.stream)
}

type handler struct {
	pubsub libbus.Messenger
	auth   middleware.AuthZReader
}

func (h *handler) stream(w http.ResponseWriter, r *http.Request) {
	if _, err := h.auth.GetIdentity(r.Context()); err != nil {
		_ = apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	requestID := strings.TrimSpace(apiframework.GetQueryParam(r, "requestId", "", "Task request ID to subscribe to."))
	if requestID == "" {
		_ = apiframework.Error(w, r, apiframework.BadRequest("requestId is required"), apiframework.GetOperation)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		_ = apiframework.Error(w, r, fmt.Errorf("streaming unsupported"), apiframework.ServerOperation)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rawCh := make(chan []byte, 32)
	sub, err := h.pubsub.Stream(r.Context(), taskengine.TaskEventRequestSubject(requestID), rawCh)
	if err != nil {
		_ = apiframework.Error(w, r, err, apiframework.GetOperation)
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-rawCh:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				log.Printf("task event SSE write failed: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
