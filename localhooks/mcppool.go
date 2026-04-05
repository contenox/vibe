package localhooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/localhooks/mcpoauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

// MCPTransport identifies how to connect to an MCP server.
type MCPTransport string

const (
	MCPTransportStdio MCPTransport = "stdio"
	MCPTransportSSE   MCPTransport = "sse"
	MCPTransportHTTP  MCPTransport = "http"
)

var (
	errMCPTransportSetupFailed   = errors.New("mcp transport setup failed")
	errMCPConnectFailed          = errors.New("mcp connect failed")
	errMCPReconnectFailed        = errors.New("mcp reconnect failed")
	errMCPToolReturnedError      = errors.New("mcp tool returned an error")
	errMCPOAuthConfigMissing     = errors.New("mcp oauth config missing")
	errMCPOAuthTokenStoreMissing = errors.New("mcp oauth token store missing")
	errMCPOAuthDiscoveryFailed   = errors.New("mcp oauth discovery failed")

	// ErrMCPSessionUnavailable indicates the pool could not establish an MCP session.
	ErrMCPSessionUnavailable = errors.New("mcp session unavailable")
	// ErrMCPToolCallFailed indicates the transport-level MCP tool call failed.
	ErrMCPToolCallFailed = errors.New("mcp tool call failed")
	// ErrMCPListToolsFailed indicates the transport-level MCP list-tools call failed.
	ErrMCPListToolsFailed = errors.New("mcp list-tools failed")
	// ErrMCPOAuthNotAuthenticated indicates the server needs `contenox mcp auth`.
	ErrMCPOAuthNotAuthenticated = errors.New("mcp oauth not authenticated")
	// ErrOAuthNotAuthenticated is kept as a backwards-compatible alias.
	ErrOAuthNotAuthenticated = ErrMCPOAuthNotAuthenticated
)

// MCPAuthType identifies the auth mechanism for remote MCP servers.
type MCPAuthType string

const (
	MCPAuthNone   MCPAuthType = ""
	MCPAuthBearer MCPAuthType = "bearer"
	MCPAuthOAuth  MCPAuthType = "oauth"
)

// MCPAuthConfig holds auth parameters for connecting to an MCP server.
type MCPAuthConfig struct {
	// Type is "bearer" or "" (none).
	Type MCPAuthType

	// Token is a literal bearer token. Prefer APIKeyFromEnv for security.
	Token string

	// APIKeyFromEnv is the name of an environment variable holding the bearer token.
	APIKeyFromEnv string
}

// ResolveToken returns the bearer token from literal value or env var.
func (a *MCPAuthConfig) ResolveToken() string {
	if a == nil {
		return ""
	}
	if a.Token != "" {
		return a.Token
	}
	if a.APIKeyFromEnv != "" {
		return os.Getenv(a.APIKeyFromEnv)
	}
	return ""
}

// MCPOAuthConfig holds parameters for the OAuth 2.1 PKCE authorization code flow.
type MCPOAuthConfig struct {
	// ClientName is registered with the authorization server (RFC 7591).
	// Defaults to "contenox".
	ClientName string

	// ClientID is a pre-registered client ID. When empty, dynamic registration
	// (RFC 7591) is attempted using the server's registration endpoint.
	ClientID string

	// Scopes to request during authorization.
	Scopes []string

	// CallbackPort is the localhost port for the redirect URI (default: 49152).
	CallbackPort int

	// TokenStore persists and retrieves OAuth tokens.
	TokenStore mcpoauth.TokenStore

	// OpenBrowser is called with the authorization URL. Defaults to launching
	// the system browser via xdg-open / open.
	OpenBrowser func(url string) error
}

type mcpError struct {
	kind    error
	message string
	cause   error
}

func newMCPError(kind error, message string, cause error) error {
	return &mcpError{
		kind:    kind,
		message: message,
		cause:   cause,
	}
}

func (e *mcpError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

func (e *mcpError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, 2)
	if e.kind != nil {
		errs = append(errs, e.kind)
	}
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// ResolveClientName returns ClientName or the default.
func (c *MCPOAuthConfig) ResolveClientName() string {
	if c.ClientName != "" {
		return c.ClientName
	}
	return "contenox"
}

// ResolveCallbackPort returns CallbackPort or the default.
func (c *MCPOAuthConfig) ResolveCallbackPort() int {
	if c.CallbackPort > 0 {
		return c.CallbackPort
	}
	return 49152
}

// MCPServerConfig describes a single MCP server connection.
type MCPServerConfig struct {
	// Name is the hook name used in chain JSON, e.g. "filesystem".
	Name string

	// Transport: "stdio" (default), "sse", or "http".
	Transport MCPTransport

	// Stdio transport: Command + Args to spawn.
	Command string
	Args    []string

	// Remote transport: URL of the SSE MCP endpoint.
	URL string

	// Auth for remote transports (optional).
	Auth *MCPAuthConfig

	// OAuth holds parameters for the OAuth 2.1 PKCE flow.
	// Only used when Auth.Type == MCPAuthOAuth.
	OAuth *MCPOAuthConfig

	// ConnectTimeout for the initial handshake (default 30s).
	ConnectTimeout time.Duration

	// MCPSessionID is the persisted session ID to resume (for HTTP/SSE transports).
	MCPSessionID string

	// OnSessionID is a callback fired when the server issues a new session ID.
	OnSessionID func(string)

	// Tracker is the activity tracker for observing MCP pool operations.
	Tracker libtracker.ActivityTracker
}

// MCPSessionPool manages a single MCP client session with reconnect support.
// Mirrors the SSHClientCache pattern: mutex-protected, reconnects on failure.
type MCPSessionPool struct {
	mu      sync.RWMutex
	session *mcp.ClientSession
	cfg     MCPServerConfig
	tracker libtracker.ActivityTracker

	// sidMu guards mcpSessionID independently of mu so the sessionRoundTripper
	// can update the live session ID (e.g. on 404 auto-heal) without deadlocking
	// against the pool-level read/write lock.
	sidMu        sync.RWMutex
	mcpSessionID string
}

// NewMCPSessionPool creates (but does not connect) a session pool for the given config.
func NewMCPSessionPool(cfg MCPServerConfig) *MCPSessionPool {
	t := cfg.Tracker
	if t == nil {
		t = libtracker.NoopTracker{}
	}
	return &MCPSessionPool{
		cfg:          cfg,
		tracker:      t,
		mcpSessionID: cfg.MCPSessionID, // seed from persisted KV value
	}
}

// Connect establishes the MCP session. Safe to call multiple times (idempotent).
func (p *MCPSessionPool) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		return nil // already connected
	}
	return p.connectLocked(ctx)
}

func (p *MCPSessionPool) connectLocked(ctx context.Context) error {
	reportErr, reportChange, end := p.tracker.Start(ctx, "connect", "mcp_server", "name", p.cfg.Name, "transport", string(p.cfg.Transport))
	defer end()

	// Per MCP spec and required by Notion: initialization requests MUST NOT
	// include an Mcp-Session-Id header. Clear any persisted session ID so the
	// sessionRoundTripper starts fresh. A new session ID will be received from
	// the server in the initialize response and persisted via OnSessionID.
	p.sidMu.Lock()
	p.mcpSessionID = ""
	p.sidMu.Unlock()

	timeout := p.cfg.ConnectTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport, err := p.buildTransport()
	if err != nil {
		userErr := newMCPError(errMCPTransportSetupFailed, fmt.Sprintf("mcp %q: transport setup failed", p.cfg.Name), err)
		reportErr(userErr)
		return userErr
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "contenox",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		userErr := newMCPError(errMCPConnectFailed, fmt.Sprintf("mcp %q: connect failed", p.cfg.Name), err)
		reportErr(userErr)
		return userErr
	}

	p.session = session
	reportChange(p.cfg.Name, map[string]any{"transport": string(p.cfg.Transport), "url": p.cfg.URL})
	return nil
}

func (p *MCPSessionPool) buildTransport() (mcp.Transport, error) {
	// Closures let sessionRoundTripper safely read/write the live session ID
	// without holding the pool-level lock (which would deadlock).
	getSessionID := func() string {
		p.sidMu.RLock()
		defer p.sidMu.RUnlock()
		return p.mcpSessionID
	}
	setSessionID := func(id string) {
		p.sidMu.Lock()
		changed := p.mcpSessionID != id
		if changed {
			p.mcpSessionID = id
		}
		p.sidMu.Unlock()
		if changed && p.cfg.OnSessionID != nil {
			p.cfg.OnSessionID(id)
		}
	}

	switch p.cfg.Transport {
	case MCPTransportStdio, "":
		if p.cfg.Command == "" {
			return nil, fmt.Errorf("stdio transport requires a command")
		}
		cmd := exec.Command(p.cfg.Command, p.cfg.Args...)
		return &mcp.CommandTransport{Command: cmd}, nil

	case MCPTransportSSE:
		if p.cfg.URL == "" {
			return nil, fmt.Errorf("sse transport requires a url")
		}
		var rt http.RoundTripper = http.DefaultTransport
		if p.cfg.Auth != nil && p.cfg.Auth.Type == MCPAuthOAuth {
			oauthRT, err := p.buildOAuthRoundTripper(rt)
			if err != nil {
				return nil, err
			}
			rt = oauthRT
		} else if token := p.cfg.Auth.ResolveToken(); token != "" {
			rt = &bearerRoundTripper{base: rt, token: token}
		}
		rt = &sessionRoundTripper{base: rt, getSessionID: getSessionID, setSessionID: setSessionID}
		t := &mcp.SSEClientTransport{Endpoint: p.cfg.URL}
		t.HTTPClient = &http.Client{Transport: rt}
		return t, nil

	case MCPTransportHTTP:
		if p.cfg.URL == "" {
			return nil, fmt.Errorf("http transport requires a url")
		}
		var rt http.RoundTripper = http.DefaultTransport
		if p.cfg.Auth != nil && p.cfg.Auth.Type == MCPAuthOAuth {
			oauthRT, err := p.buildOAuthRoundTripper(rt)
			if err != nil {
				return nil, err
			}
			rt = oauthRT
		} else if token := p.cfg.Auth.ResolveToken(); token != "" {
			rt = &bearerRoundTripper{base: rt, token: token}
		}
		rt = &sessionRoundTripper{base: rt, getSessionID: getSessionID, setSessionID: setSessionID}
		t := &mcp.StreamableClientTransport{Endpoint: p.cfg.URL}
		t.HTTPClient = &http.Client{Transport: rt}
		return t, nil

	default:
		return nil, fmt.Errorf("unknown MCP transport: %q", p.cfg.Transport)
	}
}

// Session returns the active session, connecting lazily if not yet established.
func (p *MCPSessionPool) Session(ctx context.Context) (*mcp.ClientSession, error) {
	p.mu.RLock()
	s := p.session
	p.mu.RUnlock()
	if s != nil {
		return s, nil
	}
	// Not yet connected; connect now.
	if err := p.Connect(ctx); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.session, nil
}

// Reconnect tears down the current session and re-connects.
// Called automatically by MCPHookRepo.Exec on transport errors.
func (p *MCPSessionPool) Reconnect(ctx context.Context) error {
	reportErr, reportChange, end := p.tracker.Start(ctx, "reconnect", "mcp_server", "name", p.cfg.Name)
	defer end()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		_ = p.session.Close()
		p.session = nil
	}
	reportChange(p.cfg.Name, "reconnecting")
	if err := p.connectLocked(ctx); err != nil {
		userErr := newMCPError(errMCPReconnectFailed, fmt.Sprintf("mcp %q: reconnect failed", p.cfg.Name), err)
		reportErr(userErr)
		return userErr
	}
	return nil
}

// Close terminates the session cleanly.
func (p *MCPSessionPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		err := p.session.Close()
		p.session = nil
		return err
	}
	return nil
}

// bearerRoundTripper injects Authorization: Bearer <token> on every request.
type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req2)
}

// sessionRoundTripper intercepts and injects the Mcp-Session-Id header.
// It auto-heals stale sessions: if we injected a token and the server responds
// with HTTP 404, the dead token is wiped and the request is replayed fresh.
type sessionRoundTripper struct {
	base         http.RoundTripper
	getSessionID func() string
	setSessionID func(string)
}

func (srt *sessionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	sid := srt.getSessionID()

	// Only inject if the go-sdk didn't already supply it.
	// Track whether we added it so we only auto-heal errors we caused.
	injected := false
	if sid != "" && req2.Header.Get("Mcp-Session-Id") == "" {
		req2.Header.Set("Mcp-Session-Id", sid)
		injected = true
	}

	resp, err := srt.base.RoundTrip(req2)

	// AUTO-HEAL: server returned 404 because it no longer knows this session
	// (e.g. it was restarted). Wipe the token, drain the response body to free
	// the TCP connection, and replay the original request without the header.
	if err == nil && resp != nil && resp.StatusCode == http.StatusNotFound && injected {
		// Only replay if the body is replayable (nil body or GetBody available).
		if req.Body == nil || req.GetBody != nil {
			srt.setSessionID("") // wipe in-memory + notify KV callback

			// Drain and close the 404 body to reuse the connection.
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}

			// Retry without the stale header.
			req3 := req.Clone(req.Context())
			req3.Header.Del("Mcp-Session-Id")
			if req.Body != nil && req.GetBody != nil {
				if body, bodyErr := req.GetBody(); bodyErr == nil {
					req3.Body = body
				}
			}
			resp, err = srt.base.RoundTrip(req3)
		}
	}

	// Capture server-issued session ID from any successful response.
	if err == nil && resp != nil {
		if newID := resp.Header.Get("Mcp-Session-Id"); newID != "" {
			srt.setSessionID(newID)
		}
	}
	return resp, err
}

// CallTool calls a tool on the persistent session, reconnecting once if the
// session appears to have been lost. The pool must be connected first.
func (p *MCPSessionPool) CallTool(ctx context.Context, toolName string, args map[string]any) (any, error) {
	reportErr, reportChange, end := p.tracker.Start(ctx, "call_tool", "mcp_server", "name", p.cfg.Name, "tool", toolName)
	defer end()
	if args == nil {
		args = map[string]any{}
	}
	result, err := p.callTool(ctx, toolName, args)
	if err != nil {
		if shouldReconnectAfterMCPError(err) {
			// Transport error on an established session: attempt one reconnect.
			if reconnectErr := p.Reconnect(ctx); reconnectErr != nil {
				p.reportReconnectCascade(ctx, err, reconnectErr, toolName)
				userErr := newMCPError(
					ErrMCPToolCallFailed,
					fmt.Sprintf("mcp %q.%q: call failed after reconnect attempt", p.cfg.Name, toolName),
					reconnectErr,
				)
				reportErr(userErr)
				return nil, userErr
			}
			result, err = p.callTool(ctx, toolName, args)
		}
	}
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(p.cfg.Name, toolName)
	return result, nil
}

func (p *MCPSessionPool) callTool(ctx context.Context, toolName string, args map[string]any) (any, error) {
	session, err := p.Session(ctx)
	if err != nil {
		return nil, newMCPError(
			ErrMCPSessionUnavailable,
			fmt.Sprintf("mcp %q.%q: session unavailable", p.cfg.Name, toolName),
			err,
		)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return nil, newMCPError(
			ErrMCPToolCallFailed,
			fmt.Sprintf("mcp %q.%q: call failed", p.cfg.Name, toolName),
			err,
		)
	}
	if result.IsError {
		toolErr := strings.TrimSpace(mcpCollectText(result.Content))
		if toolErr == "" {
			toolErr = "tool returned an empty error response"
		}
		return nil, newMCPError(
			errMCPToolReturnedError,
			fmt.Sprintf("mcp %q.%q: tool error", p.cfg.Name, toolName),
			errors.New(toolErr),
		)
	}
	if result.StructuredContent != nil {
		return result.StructuredContent, nil
	}
	return mcpCollectText(result.Content), nil
}

// ListTools returns all tools advertised by the MCP server, reconnecting once
// if the session has been lost.
func (p *MCPSessionPool) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	reportErr, reportChange, end := p.tracker.Start(ctx, "list_tools", "mcp_server", "name", p.cfg.Name)
	defer end()
	tools, err := p.listTools(ctx)
	if err != nil {
		if shouldReconnectAfterMCPError(err) {
			if reconnectErr := p.Reconnect(ctx); reconnectErr != nil {
				p.reportReconnectCascade(ctx, err, reconnectErr, "")
				userErr := newMCPError(
					ErrMCPListToolsFailed,
					fmt.Sprintf("mcp %q: list-tools failed after reconnect attempt", p.cfg.Name),
					reconnectErr,
				)
				reportErr(userErr)
				return nil, userErr
			}
			tools, err = p.listTools(ctx)
		}
	}
	if err != nil {
		reportErr(err)
		return nil, err
	}
	reportChange(p.cfg.Name, len(tools))
	return tools, nil
}

func (p *MCPSessionPool) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	session, err := p.Session(ctx)
	if err != nil {
		return nil, newMCPError(
			ErrMCPSessionUnavailable,
			fmt.Sprintf("mcp %q: list-tools session unavailable", p.cfg.Name),
			err,
		)
	}
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, newMCPError(
			ErrMCPListToolsFailed,
			fmt.Sprintf("mcp %q: list-tools failed", p.cfg.Name),
			err,
		)
	}
	return result.Tools, nil
}

// mcpCollectText concatenates all TextContent entries from an MCP content slice.
func mcpCollectText(contents []mcp.Content) string {
	var sb []byte
	for _, c := range contents {
		if tc, ok := c.(*mcp.TextContent); ok {
			if len(sb) > 0 {
				sb = append(sb, '\n')
			}
			sb = append(sb, tc.Text...)
		}
	}
	return string(sb)
}

// reportReconnectCascade logs the original transport error and reconnect failure on the
// activity tracker so callers can still receive a shorter named error.
func (p *MCPSessionPool) reportReconnectCascade(ctx context.Context, originalErr, reconnectErr error, toolName string) {
	kv := []any{
		"name", p.cfg.Name,
		"original_error", originalErr.Error(),
		"reconnect_error", reconnectErr.Error(),
	}
	if toolName != "" {
		kv = append(kv, "tool", toolName)
	}
	_, reportChange, end := p.tracker.Start(ctx, "mcp_reconnect_cascade", "mcp_server", kv...)
	defer end()
	reportChange(p.cfg.Name, map[string]any{
		"original_error":  originalErr.Error(),
		"reconnect_error": reconnectErr.Error(),
		"tool":            toolName,
	})
}

// isAppError determines if the error returned by the MCP SDK is an application-level
// JSON-RPC rejection (like invalid schema) or context cancellation, rather than a network drop.
func isAppError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errMCPToolReturnedError) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid params") ||
		strings.Contains(msg, "tool error") ||
		strings.Contains(msg, "unexpected additional properties") ||
		strings.Contains(msg, "missing properties") ||
		strings.Contains(msg, "method not found") ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func shouldReconnectAfterMCPError(err error) bool {
	if err == nil {
		return false
	}
	if isAppError(err) {
		return false
	}
	return !errors.Is(err, ErrMCPSessionUnavailable)
}

// buildOAuthRoundTripper constructs a non-interactive oauth2.Transport.
//
// It ONLY injects a previously-stored token and auto-refreshes it.
// It never opens a browser or binds a long-lived port — if no valid token is
// found in the store it returns ErrMCPOAuthNotAuthenticated so the caller can
// tell the user to run `contenox mcp auth <name>`.
func (p *MCPSessionPool) buildOAuthRoundTripper(base http.RoundTripper) (http.RoundTripper, error) {
	reportErr, reportChange, end := p.tracker.Start(
		context.Background(), "build_oauth_transport", "mcp_server",
		"name", p.cfg.Name,
	)
	defer end()
	ctx := context.Background()

	cfg := p.cfg.OAuth
	if cfg == nil {
		err := newMCPError(errMCPOAuthConfigMissing, "mcp oauth: config missing", nil)
		reportErr(err)
		return nil, err
	}
	if cfg.TokenStore == nil {
		err := newMCPError(errMCPOAuthTokenStoreMissing, "mcp oauth: token store missing", nil)
		reportErr(err)
		return nil, err
	}

	// Discover the authorization server (needed for token refresh endpoint).
	_, discoverReport, discoverEnd := p.tracker.Start(ctx, "oauth_discover", "mcp_server", "name", p.cfg.Name)
	meta, err := mcpoauth.DiscoverAuthServer(ctx, p.cfg.URL)
	if err != nil {
		discoverEnd()
		userErr := newMCPError(errMCPOAuthDiscoveryFailed, "mcp oauth: discover auth server", err)
		reportErr(userErr)
		return nil, userErr
	}
	discoverReport("auth_endpoint", map[string]any{
		"authorization_endpoint": meta.AuthorizationEndpoint,
		"token_endpoint":         meta.TokenEndpoint,
		"registration_endpoint":  meta.RegistrationEndpoint,
	})
	discoverEnd()

	// Resolve client ID from stored registration.
	clientID := cfg.ClientID
	if clientID == "" {
		reg, _ := cfg.TokenStore.GetClientRegistration(ctx, p.cfg.Name)
		if reg != nil {
			clientID = reg.ClientID
		}
	}

	// Load the previously stored token.
	stored, _ := cfg.TokenStore.GetOAuthToken(ctx, p.cfg.Name)

	if stored == nil || stored.AccessToken == "" {
		// No token at all — the user needs to authenticate first.
		err := newMCPError(
			ErrMCPOAuthNotAuthenticated,
			fmt.Sprintf("mcp %q: authentication required; run 'contenox mcp auth %s'", p.cfg.Name, p.cfg.Name),
			nil,
		)
		reportErr(err)
		return nil, err
	}

	reportChange("token_loaded", map[string]any{
		"client_id":   clientID,
		"token_valid": stored.Valid(),
		"has_refresh": stored.RefreshToken != "",
		"expiry":      stored.Expiry.String(),
	})

	// Build a redirect URI to satisfy oauth2.Config (only needed for refresh).
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", cfg.ResolveCallbackPort())

	o2cfg := &oauth2.Config{
		ClientID:    clientID,
		Scopes:      cfg.Scopes,
		RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{
			AuthURL:  meta.AuthorizationEndpoint,
			TokenURL: meta.TokenEndpoint,
		},
	}

	// We have a token (possibly expired but with a refresh token).
	// oauth2.ReuseTokenSource will call o2cfg.TokenSource which transparently
	// uses the refresh token when the access token has expired.
	src := oauth2.ReuseTokenSource(stored, o2cfg.TokenSource(ctx, stored))

	// Persist every newly-refreshed token so it survives process restarts.
	persistingSrc := &persistingTokenSource{
		inner:   src,
		store:   cfg.TokenStore,
		name:    p.cfg.Name,
		o2cfg:   o2cfg,
		baseCtx: ctx,
	}

	// Wrap the base transport with a logging layer that reports non-2xx
	// response bodies through the activity tracker. This surfaces things like
	// Notion's 400 Bad Request body without any changes to the user workflow.
	logged := &loggingRoundTripper{
		base:    base,
		tracker: p.tracker,
		name:    p.cfg.Name,
	}

	return &oauth2.Transport{Source: persistingSrc, Base: logged}, nil
}

// loggingRoundTripper logs non-2xx response bodies via the activity tracker
// so failures in oauth-protected requests (e.g. "Bad Request" from Notion)
// are visible through the tracing system.
type loggingRoundTripper struct {
	base    http.RoundTripper
	tracker libtracker.ActivityTracker
	name    string
}

func (l *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err != nil {
		reportErr, _, end := l.tracker.Start(req.Context(), "mcp_http_error", "mcp_server",
			"name", l.name, "url", req.URL.String(), "method", req.Method)
		defer end()
		reportErr(err)
		return resp, err
	}
	if resp != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(body)))

		detail := map[string]any{
			"url":    req.URL.String(),
			"method": req.Method,
			"status": resp.StatusCode,
			"body":   strings.TrimSpace(string(body)),
		}
		if resp.StatusCode >= 500 {
			// 5xx: server-side fault — report as error.
			reportErr, _, end := l.tracker.Start(req.Context(), "mcp_http_error", "mcp_server", "name", l.name)
			defer end()
			reportErr(fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, req.URL.Host, strings.TrimSpace(string(body))))
		} else {
			// 4xx: diagnostic — logged at INFO level as a state change so it's
			// visible in --trace output but doesn't look like a fatal error.
			// (e.g. Notion returns 405 on GET SSE stream, which the SDK handles.)
			_, reportChange, end := l.tracker.Start(req.Context(), "mcp_http_response", "mcp_server", "name", l.name)
			defer end()
			reportChange("4xx", detail)
		}
	}
	return resp, err
}

// RunOAuthFlow performs the full PKCE authorization code flow interactively:
// opens a browser at the authorization URL, waits for the callback, and
// exchanges the code for a token set. It is also used by the `mcp auth` CLI
// command when re-authenticating.
func RunOAuthFlow(ctx context.Context, o2cfg *oauth2.Config, oauthCfg *MCPOAuthConfig, meta *mcpoauth.ServerMetadata) (*oauth2.Token, error) {
	port := 0
	if oauthCfg != nil {
		port = oauthCfg.ResolveCallbackPort()
	}
	ln, redirectURI, callbackCh, err := mcpoauth.StartCallbackServer(port)
	if err != nil {
		return nil, fmt.Errorf("start callback: %w", err)
	}
	_ = ln // already serving; only used to get the port via StartCallbackServer
	o2cfg.RedirectURL = redirectURI

	verifier := oauth2.GenerateVerifier()
	state := oauth2.GenerateVerifier() // reuse randomness helper for state too

	authURL := o2cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

	openFn := openBrowserDefault
	if oauthCfg != nil && oauthCfg.OpenBrowser != nil {
		openFn = oauthCfg.OpenBrowser
	}

	clientName := "MCP server"
	if oauthCfg != nil && strings.TrimSpace(oauthCfg.ResolveClientName()) != "" {
		clientName = oauthCfg.ResolveClientName()
	}
	fmt.Printf("\nOpening browser for %s authorization...\nIf the browser doesn't open, visit:\n\n  %s\n\n", clientName, authURL)
	_ = openFn(authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-callbackCh:
		if result.Error != "" {
			return nil, fmt.Errorf("authorization denied: %s — %s", result.Error, result.ErrorDescription)
		}
		if result.State != state {
			return nil, fmt.Errorf("state mismatch: CSRF check failed")
		}
		tok, err := o2cfg.Exchange(ctx, result.Code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("token exchange: %w", err)
		}
		return tok, nil
	}
}

// persistingTokenSource wraps an oauth2.TokenSource and persists every newly-
// issued token back to the KV store, so tokens survive process restarts.
type persistingTokenSource struct {
	inner   oauth2.TokenSource
	store   mcpoauth.TokenStore
	name    string
	o2cfg   *oauth2.Config
	baseCtx context.Context
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.inner.Token()
	if err != nil {
		return nil, err
	}
	_ = s.store.SetOAuthToken(s.baseCtx, s.name, tok)
	return tok, nil
}

// openBrowserDefault opens the given URL in the system default browser.
func openBrowserDefault(rawURL string) error {
	var cmd *exec.Cmd
	switch {
	case commandExists("xdg-open"):
		cmd = exec.Command("xdg-open", rawURL)
	case commandExists("open"):
		cmd = exec.Command("open", rawURL)
	default:
		// Windows fallback
		cmd = exec.Command("cmd", "/c", "start", rawURL)
	}
	return cmd.Start()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
