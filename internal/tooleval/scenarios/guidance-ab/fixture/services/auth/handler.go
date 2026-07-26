package auth

import "net/http"

// Handler serves the auth service.
type Handler struct{ cfg Config }

// NewHandler builds a auth handler from cfg.
func NewHandler(cfg Config) *Handler { return &Handler{cfg: cfg} }

// ServeHTTP handles a auth request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = h.cfg
	w.WriteHeader(http.StatusOK)
}
