// tools_cmd.go — contenox tools subcommand tree (add, list, show, remove, update).
// Each subcommand opens only the DB; no LLM stack is needed.
package contenoxcli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contenox/beam/internal/kernel/tools"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/toolsproviderservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

// toolsCmd is the parent "contenox tools" command.
var toolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "Manage remote tool providers (add, list, show, remove, update).",
	Long: `Register and manage remote tool providers — external HTTP services exposed as LLM tools.

A remote tool provider points at an OpenAPI v3 service. When used in a chain the
runtime fetches its schema, discovers every operation, and makes them callable by
the model.

By default the spec is fetched from <url>/openapi.json. Use --spec to point at a
different location: a full URL (https://...) or a local file (~/my-spec.yaml,
./spec.json, /abs/path/spec.yaml). Local paths are stored as file:// URIs.

For APIs that require a login handshake (session-cookie or token-based auth),
use the --auth-* flags on 'tools add'. Contenox will perform the login
automatically on 401/403 and retry the call without any external refresh tooling.

Examples:
  contenox tools add myapi --url http://localhost:8080
  contenox tools add myapi --url http://localhost:8080 --header "Authorization: Bearer $TOKEN"
  contenox tools add erpnext --url https://erp.example.com --spec ~/.contenox/erp-subset.yaml
  contenox tools list
  contenox tools show myapi
  contenox tools update myapi --url http://new-host:8080
  contenox tools remove myapi`,
	SilenceUsage: true,
}

var toolsAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register a remote tool provider by name and URL.",
	Long: `Register an external OpenAPI v3 service as a named tool provider.

The runtime probes the endpoint at registration time to count available tools.
If the service is unreachable at registration, it will be retried at chain execution time.

By default the spec is fetched from <url>/openapi.json. Use --spec when the spec
lives at a different URL, or provide a local file path (~/path, ./path, /abs/path).
Local paths are resolved to absolute paths and stored as file:// URIs — the file
must exist at registration time.

Static auth: inject headers or hidden tool-call params on every request.
  --header injects an HTTP header (e.g. Authorization, X-Tenant).
  --inject injects a named parameter into every tool call, hidden from the model.

Dynamic auth (http_handshake): for APIs that require a login step before each
session (e.g. Frappe/ERPNext, legacy enterprise services with no API-key support).
When set, Contenox performs the login automatically on 401/403 and retries:
  --auth-login-url      URL to POST credentials to
  --auth-login-body     JSON body; ${ENV_VAR} placeholders are expanded at runtime
  --auth-extract-cookie Extract a named Set-Cookie value from the login response
  --auth-extract-jsonpath Extract a value via JSONPath from the login response body
  --auth-inject-header  HTTP header to carry the extracted token on API calls
  --auth-inject-format  Printf format for the value, e.g. "Bearer %s" (optional)

TLS: for services behind a private CA or self-signed certificate:
  --insecure-skip-tls-verify  Disable TLS verification for this provider only.

Examples:
  # Public API — no auth
  contenox tools add nws --url https://api.weather.gov --timeout 15000

  # Static Bearer token
  contenox tools add myapi --url https://api.example.com \
    --header "Authorization: Bearer $TOKEN" \
    --header "X-Tenant: acme" \
    --timeout 5000

  # Frappe/ERPNext — session cookie login
  contenox tools add erp --url https://erp.local \
    --insecure-skip-tls-verify \
    --auth-login-url https://erp.local/api/method/login \
    --auth-login-body '{"usr":"${FRAPPE_USER}","pwd":"${FRAPPE_PASS}"}' \
    --auth-extract-cookie sid \
    --auth-inject-header Cookie

  # Custom API — Bearer token from JSON response
  contenox tools add myapi --url https://api.example.com \
    --auth-login-url https://api.example.com/auth/token \
    --auth-login-body '{"username":"${API_USER}","password":"${API_PASS}"}' \
    --auth-extract-jsonpath '$.data.token' \
    --auth-inject-header Authorization \
    --auth-inject-format "Bearer %s"`,

	Args: cobra.ExactArgs(1),
	RunE: runToolsAdd,
}

var toolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered remote tool providers.",
	Long: `List every registered remote tool provider as a table of name, endpoint URL,
and request timeout. If none are registered, prints a hint to run
'contenox tools add'.`,
	Args: cobra.NoArgs,
	RunE: runToolsList,
}

var toolsShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details and available tools for a remote tool provider.",
	Long: `Print a provider's stored configuration — URL, spec source, timeout, TLS and
auth settings, and the keys (not values) of any headers or injected params —
then probe the live endpoint and list the tools it currently exposes. Header
and inject values are never shown. If the endpoint is unreachable, the tool
list is reported as unavailable.`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsShow,
}

var toolsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a registered remote tool provider.",
	Long: `Delete a remote tool provider by name from the local database. This removes
only the local registration; it does not affect the external service. Chains
referencing the provider will no longer resolve its tools.`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsRemove,
}

var toolsUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update an existing remote tool provider's URL, headers, timeout, or spec.",
	Long: `Update one or more properties of a registered tool provider.

Only flags that are explicitly provided are updated; others are left unchanged.

  --header replaces ALL existing headers for that provider.
  --inject replaces ALL existing inject params for that provider.
  --spec   replaces the spec source; pass an empty string to clear it
           (reverting to <url>/openapi.json discovery).

Note: auth flow (--auth-*) and TLS settings (--insecure-skip-tls-verify) can
only be set at registration time via 'tools add'. To change them, remove the
provider and re-add it with the updated flags.

Examples:
  contenox tools update myapi --url http://new-host:9090
  contenox tools update myapi --timeout 15000
  contenox tools update myapi --header "Authorization: Bearer $NEW_TOKEN"
  contenox tools update myapi --spec ~/.contenox/new-spec.yaml
  contenox tools update myapi --spec ""`,

	Args: cobra.ExactArgs(1),
	RunE: runToolsUpdate,
}

func init() {
	toolsAddCmd.Flags().String("url", "", "Base URL of the remote tools service (required)")
	_ = toolsAddCmd.MarkFlagRequired("url")
	toolsAddCmd.Flags().StringArray("header", nil, `Header to inject into every call, e.g. "Authorization: Bearer $TOKEN" (repeatable)`)
	toolsAddCmd.Flags().StringArray("inject", nil, `Param to inject as a tool call argument and hide from the model, e.g. "tenant_id=acme" (repeatable)`)
	toolsAddCmd.Flags().Int("timeout", 10000, "Request timeout in milliseconds")
	toolsAddCmd.Flags().String("spec", "", "Full URL or local file path to the OpenAPI v3 spec (e.g. https://host/openapi.yaml, ~/spec.yaml, ./spec.json)")

	// Auth flow flags
	toolsAddCmd.Flags().String("auth-login-url", "", "URL to POST credentials to before calling the API (triggers http_handshake auth flow)")
	toolsAddCmd.Flags().String("auth-login-method", "POST", "HTTP method for the login request (default: POST)")
	toolsAddCmd.Flags().String("auth-login-body", "", `JSON body for the login request, e.g. '{"usr":"${USER}","pwd":"${PASS}"}' (env vars expanded)`)
	toolsAddCmd.Flags().String("auth-extract-cookie", "", "Name of the Set-Cookie cookie to extract from the login response")
	toolsAddCmd.Flags().String("auth-extract-jsonpath", "", `JSONPath expression to extract a token from the login response body, e.g. "$.data.token"`)
	toolsAddCmd.Flags().String("auth-inject-header", "", `HTTP header to inject the extracted token into, e.g. "Cookie" or "Authorization"`)
	toolsAddCmd.Flags().String("auth-inject-format", "", `Printf format string for the injected value, e.g. "Bearer %s" or "sid=%s" (defaults to cookie "name=value" when extracting a cookie)`)
	toolsAddCmd.Flags().Bool("insecure-skip-tls-verify", false, "Skip TLS certificate verification for this provider (use only for self-signed/internal services)")

	toolsUpdateCmd.Flags().String("url", "", "New base URL")
	toolsUpdateCmd.Flags().StringArray("header", nil, `Header to inject, e.g. "Authorization: Bearer $TOKEN" (repeatable; replaces all existing headers)`)
	toolsUpdateCmd.Flags().StringArray("inject", nil, `Params to inject as tool call args (repeatable; replaces all existing injected params)`)
	toolsUpdateCmd.Flags().Int("timeout", 0, "New timeout in milliseconds (0 = keep existing)")
	toolsUpdateCmd.Flags().String("spec", "", "New spec URL or file path (replaces existing; pass empty string to clear)")

	toolsCmd.AddCommand(toolsAddCmd, toolsListCmd, toolsShowCmd, toolsRemoveCmd, toolsUpdateCmd)
}

// openToolsService resolves the DB path, opens SQLite and returns a toolsproviderservice.
// The toolsRegistry is nil here (CLI doesn't need ListLocalTools / GetSchemasForSupportedTools).
func openToolsService(cmd *cobra.Command) (libdb.DBManager, toolsproviderservice.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	return db, toolsproviderservice.New(db, nil, nil), nil
}

// parseHeaders parses a []string of "Key: Value" into a map[string]string.
func parseHeaders(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, h := range raw {
		idx := strings.Index(h, ":")
		if idx < 1 {
			return nil, fmt.Errorf("invalid header %q — expected format \"Key: Value\"", h)
		}
		key := strings.TrimSpace(h[:idx])
		val := strings.TrimSpace(h[idx+1:])
		out[key] = val
	}
	return out, nil
}

// parseInjects parses a []string of "key=value" into a map[string]string.
func parseInjects(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		idx := strings.Index(kv, "=")
		if idx < 1 {
			return nil, fmt.Errorf("invalid inject param %q — expected format \"key=value\"", kv)
		}
		key := strings.TrimSpace(kv[:idx])
		val := strings.TrimSpace(kv[idx+1:])
		out[key] = val
	}
	return out, nil
}

// probeTools fetches tools from the spec source and returns the count.
// specURL is used as the spec source when non-empty; otherwise endpointURL is used.
// Returns -1 on failure — non-fatal, just affects the registration message.
func probeTools(endpointURL, specURL string) int {
	proto := &tools.OpenAPIToolProtocol{SpecSource: specURL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	discovered, err := proto.FetchTools(ctx, endpointURL, nil, http.DefaultClient)
	if err != nil {
		return -1
	}
	return len(discovered)
}

// resolveSpecPath converts a user-supplied spec source to the canonical stored form.
//   - http:// and https:// URLs are returned as-is.
//   - file:// URIs are returned as-is (user already knows what they're doing).
//   - ~/path is expanded to the user's home directory and converted to file:///abs/path.
//   - Relative and absolute file paths are resolved to an absolute path,
//     verified to exist, and returned as file:///abs/path.
func resolveSpecPath(raw string) (string, error) {
	// Pass through URLs and already-formed file:// URIs.
	if strings.HasPrefix(raw, "http://") ||
		strings.HasPrefix(raw, "https://") ||
		strings.HasPrefix(raw, "file://") {
		return raw, nil
	}
	// Expand ~/
	if strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		raw = filepath.Join(home, raw[2:])
	}
	// Resolve to absolute path and verify it exists.
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("invalid spec path %q: %w", raw, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("spec file not found: %s", abs)
	}
	return "file://" + abs, nil
}

func runToolsAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	url, _ := cmd.Flags().GetString("url")
	rawHeaders, _ := cmd.Flags().GetStringArray("header")
	rawInjects, _ := cmd.Flags().GetStringArray("inject")
	timeoutMs, _ := cmd.Flags().GetInt("timeout")

	// Auth and TLS flags
	authLoginUrl, _ := cmd.Flags().GetString("auth-login-url")
	authLoginMethod, _ := cmd.Flags().GetString("auth-login-method")
	authLoginBody, _ := cmd.Flags().GetString("auth-login-body")
	authExtractCookie, _ := cmd.Flags().GetString("auth-extract-cookie")
	authExtractJsonPath, _ := cmd.Flags().GetString("auth-extract-jsonpath")
	authInjectHeader, _ := cmd.Flags().GetString("auth-inject-header")
	authInjectFormat, _ := cmd.Flags().GetString("auth-inject-format")
	insecureSkipVerify, _ := cmd.Flags().GetBool("insecure-skip-tls-verify")

	var authFlow *runtimetypes.AuthFlow
	if authLoginUrl != "" {
		authFlow = &runtimetypes.AuthFlow{
			Type:            "http_handshake",
			LoginURL:        authLoginUrl,
			LoginMethod:     authLoginMethod,
			LoginBody:       authLoginBody,
			ExtractCookie:   authExtractCookie,
			ExtractJSONPath: authExtractJsonPath,
			InjectHeader:    authInjectHeader,
			InjectFormat:    authInjectFormat,
		}
	}

	headers, err := parseHeaders(rawHeaders)
	if err != nil {
		return err
	}
	injectParams, err := parseInjects(rawInjects)
	if err != nil {
		return err
	}

	// Resolve optional spec source.
	var resolvedSpec string
	if specRaw, _ := cmd.Flags().GetString("spec"); specRaw != "" {
		resolved, err := resolveSpecPath(specRaw)
		if err != nil {
			return err
		}
		resolvedSpec = resolved
	}

	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openToolsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	// Check name not already taken.
	if _, err := svc.GetByName(ctx, name); err == nil {
		return fmt.Errorf("tools %q already exists; use 'contenox tools update' to modify it", name)
	}

	// Probe tools (non-fatal — purely presentation logic, not a service concern).
	toolCount := probeTools(url, resolvedSpec)

	remoteTools := &runtimetypes.RemoteTools{
		Name:               name,
		EndpointURL:        url,
		SpecURL:            resolvedSpec,
		TimeoutMs:          timeoutMs,
		Headers:            headers,
		InjectParams:       injectParams,
		AuthFlow:           authFlow,
		InsecureSkipVerify: insecureSkipVerify,
	}
	if err := svc.Create(ctx, remoteTools); err != nil {
		return fmt.Errorf("failed to register tools: %w", err)
	}

	out := cmd.OutOrStdout()
	if toolCount >= 0 {
		fmt.Fprintf(out, "Registered tools %q — %d tool(s) discovered.\n", name, toolCount)
	} else {
		fmt.Fprintf(out, "Registered tools %q — could not reach endpoint to count tools (will retry at chain execution time).\n", name)
	}
	return nil
}

func runToolsList(cmd *cobra.Command, args []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openToolsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	var all []*runtimetypes.RemoteTools
	var cursor *time.Time
	for {
		page, err := svc.List(ctx, cursor, 100)
		if err != nil {
			return fmt.Errorf("failed to list tools: %w", err)
		}
		all = append(all, page...)
		if len(page) < 100 {
			break
		}
		last := page[len(page)-1].CreatedAt
		cursor = &last
	}

	if len(all) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No remote tool providers registered. Run: contenox tools add <name> --url <endpoint>")
		return nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-20s  %-45s  %s\n", "NAME", "URL", "TIMEOUT")
	fmt.Fprintf(out, "%-20s  %-45s  %s\n", strings.Repeat("-", 20), strings.Repeat("-", 45), "-------")
	for _, h := range all {
		urlStr := h.EndpointURL
		if len(urlStr) > 45 {
			urlStr = urlStr[:42] + "..."
		}
		fmt.Fprintf(out, "%-20s  %-45s  %dms\n", h.Name, urlStr, h.TimeoutMs)
	}
	return nil
}

func runToolsShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openToolsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	remoteTools, err := svc.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("tools %q not found", name)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Name:      %s\n", remoteTools.Name)
	fmt.Fprintf(out, "URL:       %s\n", remoteTools.EndpointURL)
	if remoteTools.SpecURL != "" {
		fmt.Fprintf(out, "Spec URL:  %s\n", remoteTools.SpecURL)
	}
	fmt.Fprintf(out, "Timeout:   %dms\n", remoteTools.TimeoutMs)
	if remoteTools.InsecureSkipVerify {
		fmt.Fprintf(out, "TLS Verify:Skip\n")
	}
	if remoteTools.AuthFlow != nil {
		fmt.Fprintf(out, "Auth Flow: %s %s\n", remoteTools.AuthFlow.LoginMethod, remoteTools.AuthFlow.LoginURL)
	}
	fmt.Fprintf(out, "Registered:%s\n", remoteTools.CreatedAt.Local().Format("2006-01-02 15:04:05"))

	if len(remoteTools.Headers) > 0 {
		fmt.Fprintf(out, "Headers:   ")
		keys := make([]string, 0, len(remoteTools.Headers))
		for k := range remoteTools.Headers {
			keys = append(keys, k)
		}
		fmt.Fprintln(out, strings.Join(keys, ", ")+" (values hidden)")
	}
	if len(remoteTools.InjectParams) > 0 {
		keys := make([]string, 0, len(remoteTools.InjectParams))
		for k := range remoteTools.InjectParams {
			keys = append(keys, k)
		}
		fmt.Fprintf(out, "Inject:    %s (values hidden)\n", strings.Join(keys, ", "))
	}

	// Probe live tools — use SpecURL as spec source when set.
	proto := &tools.OpenAPIToolProtocol{SpecSource: remoteTools.SpecURL}
	toolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Build inject params from headers for probe.
	injectParams := make(map[string]tools.ParamArg, len(remoteTools.Headers))
	for k, v := range remoteTools.Headers {
		injectParams[k] = tools.ParamArg{Name: k, Value: v, In: tools.ArgLocationHeader}
	}

	fetchedTools, err := proto.FetchTools(toolCtx, remoteTools.EndpointURL, injectParams, http.DefaultClient)
	if err != nil {
		fmt.Fprintf(out, "Tools:     (could not reach endpoint: %v)\n", err)
		return nil
	}

	fmt.Fprintf(out, "Tools (%d):\n", len(fetchedTools))
	for _, t := range fetchedTools {
		// Descriptions may carry a literal "\n" that should render as a real newline.
		desc := strings.ReplaceAll(t.Function.Description, "\\n", "\n")
		fmt.Fprintf(out, "  %-30s  %s\n", t.Function.Name, desc)
	}
	return nil
}

func runToolsRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openToolsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	remoteTools, err := svc.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("tools %q not found", name)
	}
	if err := svc.Delete(ctx, remoteTools.ID); err != nil {
		return fmt.Errorf("failed to remove tools: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed tools %q.\n", name)
	return nil
}

func runToolsUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx := libtracker.WithNewRequestID(context.Background())
	db, svc, err := openToolsService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	remoteTools, err := svc.GetByName(ctx, name)
	if err != nil {
		return fmt.Errorf("tools %q not found", name)
	}

	if cmd.Flags().Changed("url") {
		remoteTools.EndpointURL, _ = cmd.Flags().GetString("url")
	}
	if cmd.Flags().Changed("timeout") {
		remoteTools.TimeoutMs, _ = cmd.Flags().GetInt("timeout")
	}
	if cmd.Flags().Changed("header") {
		rawHeaders, _ := cmd.Flags().GetStringArray("header")
		headers, err := parseHeaders(rawHeaders)
		if err != nil {
			return err
		}
		remoteTools.Headers = headers
	}
	if cmd.Flags().Changed("inject") {
		rawInjects, _ := cmd.Flags().GetStringArray("inject")
		injectParams, err := parseInjects(rawInjects)
		if err != nil {
			return err
		}
		remoteTools.InjectParams = injectParams
	}
	if cmd.Flags().Changed("spec") {
		specRaw, _ := cmd.Flags().GetString("spec")
		if specRaw == "" {
			// Explicit empty string clears the spec URL.
			remoteTools.SpecURL = ""
		} else {
			resolved, err := resolveSpecPath(specRaw)
			if err != nil {
				return err
			}
			remoteTools.SpecURL = resolved
		}
	}

	if err := svc.Update(ctx, remoteTools); err != nil {
		return fmt.Errorf("failed to update tools: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated tools %q.\n", name)
	return nil
}
