package notify

import "net/http"

// Handler serves the notify service.
type Handler struct{ cfg Config }

// NewHandler builds a notify handler from cfg.
func NewHandler(cfg Config) *Handler { return &Handler{cfg: cfg} }

// ServeHTTP handles a notify request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg
	w.WriteHeader(http.StatusOK)
}
