// tools_cmd.go — contenox tools subcommand tree (add, list, show, remove, update).
// Each subcommand opens only the DB; no LLM stack is needed.
package contenoxcli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/toolsproviderservice"
	"github.com/contenox/contenox/runtime/internal/tools"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/spf13/cobra"
)

// toolsCmd is the parent "contenox tools" command.
var toolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "Manage remote tools (add, list, show, remove, update).",
	Long: `Register and manage remote tools — external HTTP services exposed as LLM tools.

A remote tools points at an OpenAPI v3 service. When used in a chain the runtime
fetches its schema, discovers every operation, and makes them callable by the model.
The service MUST expose an OpenAPI v3 spec at its base URL.

Examples:
  contenox tools add myapi --url http://localhost:8080
  contenox tools add myapi --url http://localhost:8080 --header "Authorization: Bearer $TOKEN"
  contenox tools list
  contenox tools show myapi
  contenox tools update myapi --url http://new-host:8080
  contenox tools remove myapi`,
	SilenceUsage: true,
}

var toolsAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register a remote tools by name and URL.",
	Long: `Register an external OpenAPI v3 service as a named tools.

The runtime probes the endpoint at registration time to count available tools.
If the service is unreachable at registration, it will be retried at chain execution time.

Headers are injected into every call to the service (e.g. for authentication).
Specify each header as a separate --header flag in "Key: Value" format.

Examples:
  contenox tools add myapi --url http://localhost:8080
  contenox tools add myapi --url https://api.example.com \
    --header "Authorization: Bearer $TOKEN" \
    --header "X-Tenant: acme" \
    --timeout 5000`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsAdd,
}

var toolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered remote tools.",
	Args:  cobra.NoArgs,
	RunE:  runToolsList,
}

var toolsShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details and available tools for a remote tools.",
	Args:  cobra.ExactArgs(1),
	RunE:  runToolsShow,
}

var toolsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a registered remote tools.",
	Args:  cobra.ExactArgs(1),
	RunE:  runToolsRemove,
}

var toolsUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update an existing remote tools's URL, headers, or timeout.",
	Long: `Update one or more properties of a registered tools.

Only flags that are explicitly provided are updated; others are left unchanged.
Passing --header replaces ALL existing headers for that tools.

Examples:
  contenox tools update myapi --url http://new-host:9090
  contenox tools update myapi --timeout 15000
  contenox tools update myapi --header "Authorization: Bearer $NEW_TOKEN"`,
	Args: cobra.ExactArgs(1),
	RunE: runToolsUpdate,
}

func init() {
	toolsAddCmd.Flags().String("url", "", "Base URL of the remote tools service (required)")
	_ = toolsAddCmd.MarkFlagRequired("url")
	toolsAddCmd.Flags().StringArray("header", nil, `Header to inject into every call, e.g. "Authorization: Bearer $TOKEN" (repeatable)`)
	toolsAddCmd.Flags().StringArray("inject", nil, `Param to inject as a tool call argument and hide from the model, e.g. "tenant_id=acme" (repeatable)`)
	toolsAddCmd.Flags().Int("timeout", 10000, "Request timeout in milliseconds")

	toolsUpdateCmd.Flags().String("url", "", "New base URL")
	toolsUpdateCmd.Flags().StringArray("header", nil, `Header to inject, e.g. "Authorization: Bearer $TOKEN" (repeatable; replaces all existing headers)`)
	toolsUpdateCmd.Flags().StringArray("inject", nil, `Params to inject as tool call args (repeatable; replaces all existing injected params)`)
	toolsUpdateCmd.Flags().Int("timeout", 0, "New timeout in milliseconds (0 = keep existing)")

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

// probeTools fetches the OpenAPI schema and returns the number of tools discovered.
// Returns -1 on failure (non-fatal — we warn but still register the tools).
func probeTools(endpoint string) int {
	proto := &tools.OpenAPIToolProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := proto.FetchTools(ctx, endpoint, nil, http.DefaultClient)
	if err != nil {
		return -1
	}
	return len(tools)
}

func runToolsAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	url, _ := cmd.Flags().GetString("url")
	rawHeaders, _ := cmd.Flags().GetStringArray("header")
	rawInjects, _ := cmd.Flags().GetStringArray("inject")
	timeoutMs, _ := cmd.Flags().GetInt("timeout")

	headers, err := parseHeaders(rawHeaders)
	if err != nil {
		return err
	}
	injectParams, err := parseInjects(rawInjects)
	if err != nil {
		return err
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
	toolCount := probeTools(url)

	tools := &runtimetypes.RemoteTools{
		Name:         name,
		EndpointURL:  url,
		TimeoutMs:    timeoutMs,
		Headers:      headers,
		InjectParams: injectParams,
	}
	if err := svc.Create(ctx, tools); err != nil {
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
		fmt.Fprintln(cmd.OutOrStdout(), "No remote tools registered. Run: contenox tools add <name> --url <endpoint>")
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
	fmt.Fprintf(out, "Timeout:   %dms\n", remoteTools.TimeoutMs)
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

	// Probe live tools.
	proto := &tools.OpenAPIToolProtocol{}
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
		fmt.Fprintf(out, "  %-30s  %s\n", t.Function.Name, t.Function.Description)
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

	if err := svc.Update(ctx, remoteTools); err != nil {
		return fmt.Errorf("failed to update tools: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated tools %q.\n", name)
	return nil
}
