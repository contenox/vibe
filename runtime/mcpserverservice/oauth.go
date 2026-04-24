package mcpserverservice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/runtime/localtools"
	"github.com/contenox/contenox/runtime/localtools/mcpoauth"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"golang.org/x/oauth2"
)

const oauthPendingTTL = 15 * time.Minute
const pendingOAuthKeyPrefix = "mcp_oauth_pending:"

type OAuthStartResult struct {
	AuthorizationURL string
}

type OAuthCallbackRequest struct {
	State            string
	Code             string
	Error            string
	ErrorDescription string
}

type OAuthCallbackResult struct {
	RedirectBase string
	ServerName   string
}

type pendingOAuth struct {
	Verifier     string
	ServerName   string
	RedirectBase string
	ClientID     string
	AuthURL      string
	TokenURL     string
	CreatedAt    time.Time
}

func (s *service) AuthenticateOAuth(ctx context.Context, name string, oauthCfg *localtools.MCPOAuthConfig) error {
	srv, err := s.GetByName(ctx, name)
	if err != nil {
		return err
	}
	if err := validateOAuthServer(srv); err != nil {
		return err
	}

	tokenStore := s.oauthTokenStore()
	if oauthCfg == nil {
		oauthCfg = &localtools.MCPOAuthConfig{}
	}
	if oauthCfg.TokenStore == nil {
		oauthCfg.TokenStore = tokenStore
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", oauthCfg.ResolveCallbackPort())
	o2cfg, meta, err := s.resolveOAuthConfig(
		ctx,
		srv,
		oauthCfg.ResolveClientName(),
		strings.TrimSpace(oauthCfg.ClientID),
		redirectURI,
		oauthCfg.Scopes,
	)
	if err != nil {
		return err
	}

	tok, err := localtools.RunOAuthFlow(ctx, o2cfg, oauthCfg, meta)
	if err != nil {
		return fmt.Errorf("oauth flow: %w", err)
	}
	if err := tokenStore.SetOAuthToken(ctx, name, tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

func (s *service) StartOAuth(ctx context.Context, id, redirectBase string) (*OAuthStartResult, error) {
	srv, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateOAuthServer(srv); err != nil {
		return nil, err
	}

	redirectBase = strings.TrimSpace(redirectBase)
	if redirectBase == "" {
		redirectBase = strings.TrimSpace(s.uiBaseURL)
	}
	if redirectBase == "" {
		return nil, fmt.Errorf(`redirectBase is required (pass JSON {"redirectBase": window.location.origin}) or set UI_BASE_URL`)
	}
	redirectBase = strings.TrimRight(redirectBase, "/")
	redirectURI := redirectBase + "/api/mcp/oauth/callback"

	o2cfg, meta, err := s.resolveOAuthConfig(ctx, srv, "contenox-beam", "", redirectURI, nil)
	if err != nil {
		return nil, err
	}

	state, err := randomOAuthState()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := o2cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

	entry := pendingOAuth{
		Verifier:     verifier,
		ServerName:   srv.Name,
		RedirectBase: redirectBase,
		ClientID:     o2cfg.ClientID,
		AuthURL:      meta.AuthorizationEndpoint,
		TokenURL:     meta.TokenEndpoint,
		CreatedAt:    time.Now(),
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal oauth state: %w", err)
	}
	if err := s.store().SetKV(ctx, pendingOAuthKey(state), json.RawMessage(b)); err != nil {
		return nil, fmt.Errorf("save oauth state: %w", err)
	}

	return &OAuthStartResult{AuthorizationURL: authURL}, nil
}

func (s *service) CompleteOAuth(ctx context.Context, req OAuthCallbackRequest) (*OAuthCallbackResult, error) {
	if strings.TrimSpace(req.State) == "" {
		return nil, fmt.Errorf("state is required")
	}
	var entry pendingOAuth
	if err := s.store().GetKV(ctx, pendingOAuthKey(req.State), &entry); err != nil {
		if errors.Is(err, libdb.ErrNotFound) {
			return nil, fmt.Errorf("invalid or expired OAuth state")
		}
		return nil, fmt.Errorf("load oauth state: %w", err)
	}
	if err := s.store().DeleteKV(ctx, pendingOAuthKey(req.State)); err != nil && !errors.Is(err, libdb.ErrNotFound) {
		return nil, fmt.Errorf("delete oauth state: %w", err)
	}
	result := &OAuthCallbackResult{
		RedirectBase: entry.RedirectBase,
		ServerName:   entry.ServerName,
	}
	if time.Since(entry.CreatedAt) > oauthPendingTTL {
		return result, fmt.Errorf("oauth session expired")
	}
	if req.Error != "" {
		if req.ErrorDescription != "" {
			return result, fmt.Errorf("authorization denied: %s", req.ErrorDescription)
		}
		return result, fmt.Errorf("authorization denied: %s", req.Error)
	}
	if strings.TrimSpace(req.Code) == "" {
		return result, fmt.Errorf("code is required")
	}

	o2cfg := oauth2.Config{
		ClientID: entry.ClientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:  entry.AuthURL,
			TokenURL: entry.TokenURL,
		},
		RedirectURL: strings.TrimRight(entry.RedirectBase, "/") + "/api/mcp/oauth/callback",
	}
	tok, err := o2cfg.Exchange(ctx, req.Code, oauth2.VerifierOption(entry.Verifier))
	if err != nil {
		return result, fmt.Errorf("token exchange: %w", err)
	}
	tokenStore := mcpoauth.NewKVTokenStore(runtimetypes.New(s.db.WithoutTransaction()))
	if err := tokenStore.SetOAuthToken(ctx, entry.ServerName, tok); err != nil {
		return result, fmt.Errorf("save token: %w", err)
	}
	return result, nil
}

func (s *service) oauthTokenStore() mcpoauth.TokenStore {
	return mcpoauth.NewKVTokenStore(runtimetypes.New(s.db.WithoutTransaction()))
}

func (s *service) resolveOAuthConfig(
	ctx context.Context,
	srv *runtimetypes.MCPServer,
	clientName string,
	clientID string,
	redirectURI string,
	scopes []string,
) (*oauth2.Config, *mcpoauth.ServerMetadata, error) {
	meta, err := mcpoauth.DiscoverAuthServer(ctx, srv.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("discover oauth server: %w", err)
	}

	tokenStore := s.oauthTokenStore()
	resolvedClientID := strings.TrimSpace(clientID)
	if resolvedClientID == "" {
		if reg, err := tokenStore.GetClientRegistration(ctx, srv.Name); err == nil && reg != nil {
			resolvedClientID = reg.ClientID
		}
	}
	if resolvedClientID == "" && meta.RegistrationEndpoint != "" {
		reg, err := mcpoauth.RegisterClient(ctx, meta.RegistrationEndpoint, clientName, redirectURI)
		if err != nil {
			return nil, nil, fmt.Errorf("oauth client registration: %w", err)
		}
		if err := tokenStore.SetClientRegistration(ctx, srv.Name, reg); err != nil {
			return nil, nil, fmt.Errorf("save client registration: %w", err)
		}
		resolvedClientID = reg.ClientID
	}
	if resolvedClientID == "" {
		return nil, nil, fmt.Errorf("could not obtain OAuth client_id (dynamic registration required)")
	}

	return &oauth2.Config{
		ClientID: resolvedClientID,
		Scopes:   scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  meta.AuthorizationEndpoint,
			TokenURL: meta.TokenEndpoint,
		},
		RedirectURL: redirectURI,
	}, meta, nil
}

func validateOAuthServer(srv *runtimetypes.MCPServer) error {
	if srv.AuthType != string(localtools.MCPAuthOAuth) {
		return fmt.Errorf("mcp server %q must use authType oauth (got %q)", srv.Name, srv.AuthType)
	}
	if srv.Transport != "sse" && srv.Transport != "http" {
		return fmt.Errorf("oauth requires transport sse or http")
	}
	if strings.TrimSpace(srv.URL) == "" {
		return fmt.Errorf("mcp server has no url")
	}
	return nil
}

func randomOAuthState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func pendingOAuthKey(state string) string {
	return pendingOAuthKeyPrefix + state
}
