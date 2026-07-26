package billing

import "net/http"

// Handler serves the billing service.
type Handler struct{ cfg Config }

// NewHandler builds a billing handler from cfg.
func NewHandler(cfg Config) *Handler { return &Handler{cfg: cfg} }

// ServeHTTP handles a billing request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg
	w.WriteHeader(http.StatusOK)
}
