package backendapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	serverops "github.com/contenox/vibe/apiframework"
	"github.com/contenox/vibe/downloadservice"
	"github.com/contenox/vibe/runtimetypes"
)

func AddQueueRoutes(mux *http.ServeMux, dwService downloadservice.Service) {
	s := &downloadManager{service: dwService}
	mux.HandleFunc("GET /queue", s.getQueue)
	mux.HandleFunc("DELETE /queue/{model}", s.removeFromQueue)
	mux.HandleFunc("GET /queue/inProgress", s.inProgress)
	mux.HandleFunc("DELETE /queue/cancel", s.cancelDownload)
}

type downloadManager struct {
	service downloadservice.Service
}

// Retrieves the current model download queue state.
//
// Returns a list of models waiting to be downloaded.
// Downloading models is only supported for ollama backends.
// If groups are enabled, models will only be downloaded to backends
// that are associated with at least one group.
func (s *downloadManager) getQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	currentQueue, err := s.service.CurrentDownloadQueueState(ctx)
	if err != nil {
		_ = serverops.Error(w, r, err, serverops.GetOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, currentQueue) // @response []downloadservice.Job
}

// Removes a model from the download queue.
// If a model download is in progress or the download will be cancelled.
func (s *downloadManager) removeFromQueue(w http.ResponseWriter, r *http.Request) {
	modelName := serverops.GetPathParam(r, "model", "The name of the model to remove from the queue (e.g., 'mistral:latest').")
	if modelName == "" {
		_ = serverops.Error(w, r, fmt.Errorf("missing model parameter %w", serverops.ErrBadPathValue), serverops.DeleteOperation)
		return
	}

	if err := s.service.RemoveDownloadFromQueue(r.Context(), modelName); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "Model removed from queue") // @response string
}

// Streams real-time download progress via Server-Sent Events (SSE).
//
// Clients should handle 'data' events containing JSON status updates.
// Connection remains open until client disconnects or server closes.
// Example event format:
// event: status
// data: {"status":"downloading","digest":"sha256:abc123","total":1000000,"completed":250000,"model":"mistral:latest","baseUrl":"http://localhost:11434"}
func (s *downloadManager) inProgress(w http.ResponseWriter, r *http.Request) {
	// Set appropriate SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Ensure the ResponseWriter supports flushing.
	flusher, ok := w.(http.Flusher)
	if !ok {
		serverops.Error(w, r, fmt.Errorf("streaming unsupported"), serverops.ServerOperation)
		return
	}

	// Create a channel to receive progress statuses.
	statusCh := make(chan *runtimetypes.Status)

	// Use a separate goroutine to subscribe and push updates into statusCh.
	go func() {
		if err := s.service.DownloadInProgress(r.Context(), statusCh); err != nil {
			log.Printf("error during InProgress subscription: %v", err)
		}
		// When InProgress returns (e.g. context canceled), close the channel.
		close(statusCh)
	}()

	// Listen for incoming status updates and stream them to the client.
	for {
		select {
		case st, ok := <-statusCh:
			if !ok {
				// Channel closed: end the stream.
				return
			}
			// Marshal the status update into JSON.
			data, err := json.Marshal(st)
			if err != nil {
				log.Printf("failed to marshal status update: %v", err)
				continue
			}
			// Write the SSE formatted message.
			fmt.Fprintf(w, "data: %s\n\n", data)
			// Flush to ensure the message is sent immediately.
			flusher.Flush()
		case <-r.Context().Done():
			// Client canceled the request.
			return
		}
	}

}

// Cancels an in-progress model download.
//
// Accepts either:
// - 'url' query parameter to cancel a download on a specific backend
// - 'model' query parameter to cancel the model download across all backends
// Example: /queue/cancel?url=http://localhost:11434
//
//	/queue/cancel?model=mistral:latest
func (s *downloadManager) cancelDownload(w http.ResponseWriter, r *http.Request) {
	// Use helpers to document both possible query parameters
	urlParam := serverops.GetQueryParam(r, "url", "", "The base URL of a specific backend to cancel downloads on.")
	modelParam := serverops.GetQueryParam(r, "model", "", "The model name to cancel downloads for across all backends.")

	// Prioritize the 'url' parameter if it exists
	valueToCancel := urlParam
	if valueToCancel == "" {
		valueToCancel = modelParam
	}

	if valueToCancel == "" {
		err := fmt.Errorf("%w: required query parameter 'url' or 'model' is missing", serverops.ErrBadPathValue)
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	if err := s.service.CancelDownloads(r.Context(), valueToCancel); err != nil {
		_ = serverops.Error(w, r, err, serverops.DeleteOperation)
		return
	}

	_ = serverops.Encode(w, r, http.StatusOK, "Model download cancellation initiated") // @response string
}
