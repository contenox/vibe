// Package mcpoauth implements the MCP OAuth 2.1 Authorization Code + PKCE
// flow for CLI clients.
//
// It covers three responsibilities the golang.org/x/oauth2 package does not:
//
//  1. Server metadata discovery (RFC 8414):
//     fetches /.well-known/oauth-authorization-server to locate the
//     authorization and token endpoints.
//
//  2. Dynamic client registration (RFC 7591):
//     registers a new OAuth client with the authorization server on first use.
//
//  3. Local callback server:
//     starts a temporary localhost HTTP server to receive the authorization
//     code redirect, then shuts it down.
//
// golang.org/x/oauth2 handles PKCE, token exchange, token refresh, and the
// Transport RoundTripper.
package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ServerMetadata holds the subset of RFC 8414 fields we need.
type ServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// SupportsS256 reports whether the server advertises S256 PKCE support.
// If the field is absent we assume yes (most modern servers support it).
func (m *ServerMetadata) SupportsS256() bool {
	if len(m.CodeChallengeMethodsSupported) == 0 {
		return true
	}
	for _, method := range m.CodeChallengeMethodsSupported {
		if method == "S256" {
			return true
		}
	}
	return false
}

// DiscoverAuthServer fetches the OAuth 2.0 Authorization Server Metadata
// (RFC 8414) for the given MCP server URL.
//
// It first tries the well-known URL derived from the server's base origin,
// then falls back to conventional endpoint paths if the server returns 404.
func DiscoverAuthServer(ctx context.Context, mcpServerURL string) (*ServerMetadata, error) {
	u, err := url.Parse(mcpServerURL)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: parse server URL: %w", err)
	}
	baseOrigin := u.Scheme + "://" + u.Host

	// RFC 8414 §3 — well-known endpoint.
	wellKnown := baseOrigin + "/.well-known/oauth-authorization-server"
	meta, err := fetchJSON[ServerMetadata](ctx, wellKnown)
	if err == nil {
		return meta, nil
	}

	// Fallback: synthesize from the base origin.
	return &ServerMetadata{
		Issuer:                baseOrigin,
		AuthorizationEndpoint: baseOrigin + "/authorize",
		TokenEndpoint:         baseOrigin + "/token",
		RegistrationEndpoint:  baseOrigin + "/register",
	}, nil
}

// ClientRegistration is the result of a successful dynamic client registration.
type ClientRegistration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"` // absent for public clients
}

// RegisterClient performs RFC 7591 Dynamic Client Registration against the
// given endpoint. It registers a public client (no secret) suitable for a
// native CLI application.
//
// If registrationEndpoint is empty the caller must supply a clientID directly.
func RegisterClient(ctx context.Context, registrationEndpoint, clientName, redirectURI string) (*ClientRegistration, error) {
	if registrationEndpoint == "" {
		return nil, fmt.Errorf("mcpoauth: no registration endpoint — supply --oauth-client-id manually")
	}

	body := map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none", // public client
		"application_type":           "native",
	}
	b, _ := json.Marshal(body)

	reg, err := postJSON[ClientRegistration](ctx, registrationEndpoint, b)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: register client: %w", err)
	}
	return reg, nil
}

// CallbackResult is the outcome of the local callback server.
type CallbackResult struct {
	Code  string
	State string
	Error string
	ErrorDescription string
}

// StartCallbackServer binds a local HTTP listener on the given port (or a
// random port if port == 0) and returns the listener, its redirect URI, and a
// channel that will receive exactly one CallbackResult when the browser
// redirects to it.
//
// The caller is responsible for closing the listener when done.
func StartCallbackServer(port int) (net.Listener, string, <-chan CallbackResult, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", nil, fmt.Errorf("mcpoauth: listen on %s: %w", addr, err)
	}

	ch := make(chan CallbackResult, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		result := CallbackResult{
			Code:             q.Get("code"),
			State:            q.Get("state"),
			Error:            q.Get("error"),
			ErrorDescription: q.Get("error_description"),
		}

		if result.Error != "" {
			http.Error(w, "Authorization failed: "+result.ErrorDescription, http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, successPage)
		}

		ch <- result
		// Shut the server down asynchronously so this handler can return.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})

	go func() { _ = srv.Serve(ln) }()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	return ln, redirectURI, ch, nil
}

// ── internal helpers ──────────────────────────────────────────────────────────

func fetchJSON[T any](ctx context.Context, rawURL string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func postJSON[T any](ctx context.Context, rawURL string, body []byte) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out T
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

const successPage = `<!DOCTYPE html><html><head><meta charset="utf-8">
<title>contenox — authenticated</title>
<style>
  body { font-family: system-ui, sans-serif; display:flex; align-items:center;
         justify-content:center; height:100vh; margin:0; background:#f5f5f5; }
  .card { background:#fff; border-radius:12px; padding:2rem 3rem;
          box-shadow:0 4px 24px rgba(0,0,0,.08); text-align:center; }
  h1 { margin:0 0 .5rem; font-size:1.5rem; color:#1a1a1a; }
  p  { margin:0; color:#666; }
  .check { font-size:3rem; margin-bottom:1rem; }
</style>
</head><body>
<div class="card">
  <div class="check">✅</div>
  <h1>Authenticated!</h1>
  <p>You can close this tab and return to the terminal.</p>
</div>
</body></html>`
