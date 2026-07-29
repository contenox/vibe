package localtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/libtracker"
	"github.com/getkin/kin-openapi/openapi3"
)

// WebToolsName is the namespace under which all web_* verb tools are exposed
// and the key used to look up tools_policies entries.
const WebToolsName = "webtools"

// WebCaller exposes per-verb HTTP tools (web_get, web_head, web_post, web_put,
// web_patch, web_delete) under the "webtools" namespace. Each call is gated
// by tools_policies.webtools (host allow/deny, scheme allowlist, size limits,
// timeout, retry, redirects), tracked through libtracker, and — for mutating
// verbs — gated by HITL approval per the default policy.
type WebCaller struct {
	client         *http.Client
	defaultHeaders map[string]string
	tracker        libtracker.ActivityTracker
}

// NewWebCaller creates a new WebCaller. Pass nil for tracker to disable
// tracing; the constructor swaps it for a NoopTracker so the call sites stay
// uniform.
func NewWebCaller(tracker libtracker.ActivityTracker, options ...WebtoolsOption) taskengine.ToolsRepo {
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	wh := &WebCaller{
		client: &http.Client{Timeout: 30 * time.Second},
		defaultHeaders: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
		},
		tracker: tracker,
	}
	for _, opt := range options {
		opt(wh)
	}
	return wh
}

type WebtoolsOption func(*WebCaller)

func WithHTTPClient(client *http.Client) WebtoolsOption {
	return func(h *WebCaller) { h.client = client }
}

func WithDefaultHeader(key, value string) WebtoolsOption {
	return func(h *WebCaller) { h.defaultHeaders[key] = value }
}

// ── policy readers ────────────────────────────────────────────────────────────

func (h *WebCaller) policyArgs(ctx context.Context) map[string]string {
	return taskengine.ToolsArgsFromContext(ctx, WebToolsName)
}

func (h *WebCaller) policyCSV(args map[string]string, key, fallback string) []string {
	raw, present := args[key]
	if !present {
		raw = fallback
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (h *WebCaller) policyInt(args map[string]string, key string, fallback int) int {
	s := strings.TrimSpace(args[key])
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func (h *WebCaller) policyBool(args map[string]string, key string, fallback bool) bool {
	s := strings.ToLower(strings.TrimSpace(args[key]))
	switch s {
	case "":
		return fallback
	case "true", "1", "yes", "y":
		return true
	case "false", "0", "no", "n":
		return false
	}
	return fallback
}

// ── URL / host validation ─────────────────────────────────────────────────────

// validateURL parses raw and enforces scheme + host policies. Returns the
// parsed URL on success or a soft denial string (with denial=true) on policy
// violation. denial=true means "return this string to the model as a tool
// result"; denial=false with err set means a hard error (malformed URL).
func (h *WebCaller) validateURL(args map[string]string, raw string) (*url.URL, string, bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", false, fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	allowedSchemes := h.policyCSV(args, "_allowed_schemes", "http,https")
	if !contains(allowedSchemes, scheme) {
		return nil, fmt.Sprintf("webtools: scheme %q is not in allowed schemes %v", scheme, allowedSchemes), true, nil
	}
	host := u.Hostname()
	if host == "" {
		return nil, "webtools: URL must include a host", true, nil
	}
	// No host is denied by default; host policy is opt-in, either via this
	// _denied_hosts knob or — preferably — an HITL policy rule with op:"host".
	denied := h.policyCSV(args, "_denied_hosts", "")
	if hostMatches(host, denied) {
		return nil, fmt.Sprintf("webtools: host %q is denied by tools_policies.webtools._denied_hosts", host), true, nil
	}
	allowed := h.policyCSV(args, "_allowed_hosts", "")
	if len(allowed) > 0 && !hostMatches(host, allowed) {
		return nil, fmt.Sprintf("webtools: host %q is not in allowed hosts %v", host, allowed), true, nil
	}
	return u, "", false, nil
}

// hostMatches checks whether host equals or is a subdomain of any pattern in
// patterns. A pattern of "example.com" matches "example.com" and "api.example.com".
// IP literals match exactly. Empty list = no match.
func hostMatches(host string, patterns []string) bool {
	host = strings.ToLower(host)
	if ip := net.ParseIP(host); ip != nil {
		for _, p := range patterns {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == host {
				return true
			}
		}
		return false
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if host == p || strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	v = strings.ToLower(v)
	for _, x := range list {
		if strings.ToLower(strings.TrimSpace(x)) == v {
			return true
		}
	}
	return false
}

// ── arg extraction ────────────────────────────────────────────────────────────

func (h *WebCaller) extractURL(input map[string]any, toolsCall *taskengine.ToolsCall) (string, error) {
	if v, ok := input["url"].(string); ok && v != "" {
		return v, nil
	}
	if toolsCall != nil && toolsCall.Args != nil {
		if v := toolsCall.Args["url"]; v != "" {
			return v, nil
		}
	}
	return "", errors.New("missing 'url' argument")
}

func (h *WebCaller) extractHeaders(input map[string]any, toolsCall *taskengine.ToolsCall) (map[string]string, error) {
	out := map[string]string{}
	v, ok := input["headers"]
	if !ok {
		if toolsCall != nil && toolsCall.Args != nil {
			if s := toolsCall.Args["headers"]; s != "" {
				v = s
				ok = true
			}
		}
	}
	if !ok {
		return out, nil
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			out[k] = fmt.Sprintf("%v", val)
		}
	case map[string]string:
		for k, val := range x {
			out[k] = val
		}
	case string:
		if x == "" {
			return out, nil
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			return nil, fmt.Errorf("headers: invalid JSON-string: %w", err)
		}
		for k, val := range m {
			out[k] = val
		}
	default:
		return nil, fmt.Errorf("headers: unsupported type %T", v)
	}
	return out, nil
}

func (h *WebCaller) extractQuery(input map[string]any, toolsCall *taskengine.ToolsCall) string {
	if v, ok := input["query"].(string); ok {
		return v
	}
	if toolsCall != nil && toolsCall.Args != nil {
		return toolsCall.Args["query"]
	}
	return ""
}

func (h *WebCaller) extractBody(input map[string]any, maxBytes int) (io.Reader, int, error) {
	v, ok := input["body"]
	if !ok || v == nil {
		return nil, 0, nil
	}
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to marshal body: %w", err)
		}
		raw = b
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		return nil, 0, fmt.Errorf("request body is %d bytes (max %d); raise tools_policies.webtools._max_request_body_bytes or shrink the body", len(raw), maxBytes)
	}
	return bytes.NewReader(raw), len(raw), nil
}

// ── core request loop ─────────────────────────────────────────────────────────

// doRequest is the per-verb dispatch. It validates the URL, builds the
// request, runs the retry/backoff loop, reads the size-limited response, and
// closes the tracker span. On a soft policy denial (host/scheme/size) it
// returns (denialString, DataTypeString, nil) so the model sees a tool result.
func (h *WebCaller) doRequest(ctx context.Context, method, toolName string, input any, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	dynArgs, _ := input.(map[string]any)
	if dynArgs == nil {
		dynArgs = map[string]any{}
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		if err := rejectUnknownArgs("webtools."+toolName, dynArgs, "url", "headers", "query"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	default:
		if err := rejectUnknownArgs("webtools."+toolName, dynArgs, "url", "headers", "query", "body"); err != nil {
			return nil, taskengine.DataTypeAny, err
		}
	}

	policy := h.policyArgs(ctx)
	timeoutSec := h.policyInt(policy, "_request_timeout_seconds", 30)
	maxRespBytes := h.policyInt(policy, "_max_response_bytes", 1<<20)
	maxBodyBytes := h.policyInt(policy, "_max_request_body_bytes", 256<<10)
	maxAttempts := h.policyInt(policy, "_max_attempts", 3)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	initialBackoffMs := h.policyInt(policy, "_initial_backoff_ms", 250)
	maxBackoffMs := h.policyInt(policy, "_max_backoff_ms", 5000)
	disallowRedirects := h.policyBool(policy, "_disallow_redirects", false)

	rawURL, err := h.extractURL(dynArgs, toolsCall)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	u, denial, denied, err := h.validateURL(policy, rawURL)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}
	if denied {
		return denial, taskengine.DataTypeString, nil
	}

	if q := h.extractQuery(dynArgs, toolsCall); q != "" {
		extra, err := url.ParseQuery(q)
		if err != nil {
			return nil, taskengine.DataTypeAny, fmt.Errorf("invalid query parameters: %w", err)
		}
		existing := u.Query()
		for k, vals := range extra {
			for _, v := range vals {
				existing.Add(k, v)
			}
		}
		u.RawQuery = existing.Encode()
	}

	headers, err := h.extractHeaders(dynArgs, toolsCall)
	if err != nil {
		return nil, taskengine.DataTypeAny, err
	}

	host := u.Hostname()
	reportErr, reportChange, end := h.tracker.Start(ctx, "exec", toolName, "url", u.String(), "host", host, "method", method)
	defer end()

	// Build a per-call client so timeout and redirect policy come from
	// tools_policies, not the shared default client.
	client := *h.client
	client.Timeout = time.Duration(timeoutSec) * time.Second
	if disallowRedirects {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	var (
		respBody   []byte
		statusCode int
		truncated  bool
	)
	backoff := time.Duration(initialBackoffMs) * time.Millisecond
	maxBackoff := time.Duration(maxBackoffMs) * time.Millisecond
	if maxBackoff < backoff {
		maxBackoff = backoff
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Body must be re-built per attempt because io.Reader is single-use.
		body, _, bodyErr := h.extractBody(dynArgs, maxBodyBytes)
		if bodyErr != nil {
			reportErr(bodyErr)
			return nil, taskengine.DataTypeAny, bodyErr
		}

		req, reqErr := http.NewRequestWithContext(ctx, method, u.String(), body)
		if reqErr != nil {
			reportErr(reqErr)
			return nil, taskengine.DataTypeAny, fmt.Errorf("failed to create request: %w", reqErr)
		}
		for k, v := range h.defaultHeaders {
			req.Header.Set(k, v)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, doErr := client.Do(req)
		if doErr != nil {
			if attempt < maxAttempts {
				time.Sleep(jitter(backoff))
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			reportErr(doErr)
			return nil, taskengine.DataTypeAny, fmt.Errorf("request failed after %d attempts: %w", attempt, doErr)
		}

		statusCode = resp.StatusCode
		var readErr error
		respBody, truncated, readErr = readLimited(resp.Body, maxRespBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			reportErr(readErr)
			return nil, taskengine.DataTypeAny, fmt.Errorf("failed to read response: %w", readErr)
		}
		if statusCode >= 500 && attempt < maxAttempts {
			time.Sleep(jitter(backoff))
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		break
	}

	if statusCode >= 200 && statusCode < 300 {
		reportChange(fmt.Sprintf("status_%d", statusCode), nil)
		var parsed any
		if json.Valid(respBody) {
			if err := json.Unmarshal(respBody, &parsed); err == nil {
				if truncated {
					return wrapTruncated(parsed, len(respBody), maxRespBytes), taskengine.DataTypeJSON, nil
				}
				return parsed, taskengine.DataTypeJSON, nil
			}
		}
		out := string(respBody)
		if truncated {
			out += fmt.Sprintf("\n\n[response truncated to %d bytes; raise tools_policies.webtools._max_response_bytes to read more]", maxRespBytes)
		}
		return out, taskengine.DataTypeString, nil
	}

	failure := fmt.Errorf("webtools %s %s: HTTP %d: %s", method, u.String(), statusCode, truncatedTail(respBody, 512))
	reportErr(failure)
	return nil, taskengine.DataTypeAny, failure
}

func wrapTruncated(parsed any, n, max int) any {
	return map[string]any{
		"_truncated":  true,
		"_bytes_read": n,
		"_max_bytes":  max,
		"body":        parsed,
	}
}

func truncatedTail(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func readLimited(r io.Reader, max int) ([]byte, bool, error) {
	if max <= 0 {
		all, err := io.ReadAll(r)
		return all, false, err
	}
	buf := make([]byte, 0, max)
	tmp := make([]byte, 4096)
	truncated := false
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			remaining := max - len(buf)
			if n > remaining {
				buf = append(buf, tmp[:remaining]...)
				truncated = true
				_, _ = io.Copy(io.Discard, r)
				break
			}
			buf = append(buf, tmp[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return buf, truncated, err
		}
	}
	return buf, truncated, nil
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	return next
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// Up to ±25% jitter.
	r := rand.Float64()*0.5 - 0.25
	return d + time.Duration(float64(d)*r)
}

// ── ToolsRepo surface ─────────────────────────────────────────────────────────

// Exec dispatches to one of the verb-specific handlers based on toolsCall.ToolName.
func (h *WebCaller) Exec(ctx context.Context, _ time.Time, input any, _ bool, toolsCall *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if toolsCall == nil {
		return nil, taskengine.DataTypeAny, errors.New("webtools: tools_call required")
	}
	toolName := toolsCall.ToolName
	if toolName == "" {
		toolName = toolsCall.Name
	}
	switch toolName {
	case "web_get":
		return h.doRequest(ctx, http.MethodGet, toolName, input, toolsCall)
	case "web_head":
		return h.doRequest(ctx, http.MethodHead, toolName, input, toolsCall)
	case "web_post":
		return h.doRequest(ctx, http.MethodPost, toolName, input, toolsCall)
	case "web_put":
		return h.doRequest(ctx, http.MethodPut, toolName, input, toolsCall)
	case "web_patch":
		return h.doRequest(ctx, http.MethodPatch, toolName, input, toolsCall)
	case "web_delete":
		return h.doRequest(ctx, http.MethodDelete, toolName, input, toolsCall)
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("webtools: unknown tool %q", toolName)
	}
}

func (h *WebCaller) Supports(_ context.Context) ([]string, error) {
	return []string{WebToolsName, "web_get", "web_head", "web_post", "web_put", "web_patch", "web_delete"}, nil
}

func (h *WebCaller) GetSchemasForSupportedTools(_ context.Context) (map[string]*openapi3.T, error) {
	return map[string]*openapi3.T{}, nil
}

// readVerbProps returns the JSON schema parameters for read-only verbs (GET, HEAD).
func readVerbProps() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "Absolute URL to call. Scheme must be in tools_policies.webtools._allowed_schemes (default http,https). Host must pass _allowed_hosts / _denied_hosts policy."},
			"headers": map[string]any{"type": "object", "description": "Optional HTTP headers as a JSON object {\"X-Foo\":\"bar\"}. A JSON-encoded string is also accepted for back-compat.", "additionalProperties": map[string]any{"type": "string"}},
			"query":   map[string]any{"type": "string", "description": "Optional URL-encoded query string (e.g. \"a=1&b=2\"). Merged with the URL's existing query if present."},
		},
		"required": []string{"url"},
	}
}

// writeVerbProps returns the schema for verbs that may carry a request body.
func writeVerbProps() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url":     map[string]any{"type": "string", "description": "Absolute URL to call. Subject to _allowed_schemes and host allow/deny policy."},
			"headers": map[string]any{"type": "object", "description": "Optional HTTP headers as a JSON object {\"X-Foo\":\"bar\"}.", "additionalProperties": map[string]any{"type": "string"}},
			"query":   map[string]any{"type": "string", "description": "Optional URL-encoded query string."},
			"body": map[string]any{
				"type":        []any{"string", "number", "integer", "boolean", "object", "array", "null"},
				"description": "Request body. A string is sent as-is; any other JSON value is marshalled. Capped by tools_policies.webtools._max_request_body_bytes (default 256 KiB).",
			},
		},
		"required": []string{"url"},
	}
}

func (h *WebCaller) GetToolsForToolsByName(_ context.Context, name string) ([]taskengine.Tool, error) {
	all := []taskengine.Tool{
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_get",
			Description: "Make an HTTP GET request. Use for read-only retrieval. Response is parsed as JSON when possible, otherwise returned as text. Subject to host allow/deny policy and a response-size cap (default 1 MiB).",
			Parameters:  readVerbProps(),
		}},
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_head",
			Description: "Make an HTTP HEAD request. Use to inspect headers and status without fetching the body.",
			Parameters:  readVerbProps(),
		}},
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_post",
			Description: "Make an HTTP POST request. Triggers a HITL approval prompt by default — the user sees the URL and method before the request is sent. Body capped by _max_request_body_bytes.",
			Parameters:  writeVerbProps(),
		}},
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_put",
			Description: "Make an HTTP PUT request. Triggers a HITL approval prompt by default.",
			Parameters:  writeVerbProps(),
		}},
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_patch",
			Description: "Make an HTTP PATCH request. Triggers a HITL approval prompt by default.",
			Parameters:  writeVerbProps(),
		}},
		{Type: "function", Function: taskengine.FunctionTool{
			Name:        "web_delete",
			Description: "Make an HTTP DELETE request. Triggers a HITL approval prompt by default.",
			Parameters:  writeVerbProps(),
		}},
	}
	if name == WebToolsName {
		return all, nil
	}
	for _, t := range all {
		if t.Function.Name == name {
			return []taskengine.Tool{t}, nil
		}
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

var _ taskengine.ToolsRepo = (*WebCaller)(nil)
