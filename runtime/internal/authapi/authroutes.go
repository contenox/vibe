// Package usersapi provides HTTP handlers for user authentication and management.
package usersapi

import (
	"fmt"
	"net/http"
	"time"

	apiframework "github.com/contenox/contenox/apiframework"
	"github.com/contenox/contenox/apiframework/middleware"
)

const (
	authCookieName = "auth_token"
)

func AddAuthRoutes(mux *http.ServeMux, loginManager middleware.LoginManager, tokenManager middleware.AuthzManager) {
	a := &authManager{
		loginManager: loginManager,
		tokenManager: tokenManager,
	}

	mux.HandleFunc("POST /login", a.login) // Resource Owner Password Credentials Flow (M2M & BfF only)
	mux.HandleFunc("POST /token_refresh", a.tokenRefresh)

	mux.HandleFunc("GET /ui/me", a.uiMe)
	mux.HandleFunc("POST /ui/login", a.uiLogin)
	mux.HandleFunc("POST /ui/logout", a.uiLogout)
	mux.HandleFunc("POST /ui/token_refresh", a.uiTokenRefresh)
}

type authManager struct {
	loginManager middleware.LoginManager
	tokenManager middleware.AuthzManager
}

// LoginRequest represents credentials for programmatic login.
type LoginRequest struct {
	Email    string `json:"email" example:"user@example.com"`
	Password string `json:"password" example:"s3cr3t!"`
}

// Authenticates a user using email and password (for machine-to-machine or backend-for-frontend flows).
//
// Returns a JWT token and user details on success.
// WARNING: Do not use this endpoint directly from browser-based clients.
func (a *authManager) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := apiframework.Decode[LoginRequest](r) // @request usersapi.LoginRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	result, err := a.loginManager.Login(ctx, req.Email, req.Password)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, result) // @response middleware.LoginResponse
}

// tokenRefreshRequest contains the token to be refreshed.
type tokenRefreshRequest struct {
	Token string `json:"token" example:"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."`
}

// tokenRefreshResponse contains the new token.
type tokenRefreshResponse struct {
	Token string `json:"token" example:"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."`
}

// Refreshes an expired or expiring JWT token.
//
// Accepts a valid token and returns a new one with extended expiration.
func (a *authManager) tokenRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := apiframework.Decode[tokenRefreshRequest](r) // @request usersapi.tokenRefreshRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	newToken, _, _, err := a.tokenManager.RefreshToken(ctx, req.Token, nil)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	resp := tokenRefreshResponse{Token: newToken}
	apiframework.Encode(w, r, http.StatusOK, resp) // @response usersapi.tokenRefreshResponse
}

// Returns the currently authenticated user (for UI clients).
//
// Requires a valid authentication cookie.
func (a *authManager) uiMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, err := middleware.GetLoginResponse(ctx, a.tokenManager)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	apiframework.Encode(w, r, http.StatusOK, user) // @response middleware.LoginResponse
}

// Authenticates a user and sets an HTTP-only authentication cookie (for UI clients).
//
// The cookie is secure, HTTP-only, and has a strict SameSite policy.
func (a *authManager) uiLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := apiframework.Decode[LoginRequest](r) // @request usersapi.LoginRequest
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	result, err := a.loginManager.Login(ctx, req.Email, req.Password)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	// Set secure HTTP-only cookie
	cookie := &http.Cookie{
		Name:     authCookieName,
		Value:    result.Token,
		Path:     "/",
		Expires:  result.ExpiresAt,
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
		Secure:   r.TLS != nil, // Automatically use HTTPS in prod
	}
	http.SetCookie(w, cookie)

	resp := mapToResponse(result)

	apiframework.Encode(w, r, http.StatusOK, resp) // @response usersapi.LoginResponse
}

// Clears the authentication cookie and logs the user out.
func (a *authManager) uiLogout(w http.ResponseWriter, r *http.Request) {
	// Expire the cookie
	cookie := &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
		Secure:   r.TLS != nil,
	}
	http.SetCookie(w, cookie)

	apiframework.Encode(w, r, http.StatusOK, "logout successful") // @response string
}

// Refreshes the authentication token stored in the cookie (for UI clients).
//
// Reads the current token from the cookie, refreshes it, and updates the cookie.
func (a *authManager) uiTokenRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	cookie, err := r.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		apiframework.Error(w, r, fmt.Errorf("authentication cookie missing: %w", apiframework.ErrUnauthorized), apiframework.AuthorizeOperation)
		return
	}

	newToken, _, expiresAt, err := a.tokenManager.RefreshToken(ctx, cookie.Value, nil)
	if err != nil {
		apiframework.Error(w, r, err, apiframework.AuthorizeOperation)
		return
	}

	// Update the cookie
	newCookie := &http.Cookie{
		Name:     authCookieName,
		Value:    newToken,
		Path:     "/",
		Expires:  expiresAt,
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
		Secure:   r.TLS != nil,
	}
	http.SetCookie(w, newCookie)

	apiframework.Encode(w, r, http.StatusOK, "token refreshed") // @response string
}

type LoginResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
}

func mapToResponse(resp middleware.LoginResponse) LoginResponse {
	return LoginResponse{
		ExpiresAt: resp.ExpiresAt,
		UserID:    resp.UserID,
		Username:  resp.Username,
	}
}
