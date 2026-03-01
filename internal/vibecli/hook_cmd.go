// hook_cmd.go — vibe hook subcommand tree (add, list, show, remove, update).
// Each subcommand opens only the DB; no LLM stack is needed.
package vibecli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/vibe/internal/hooks"
	"github.com/contenox/vibe/runtimetypes"
	"github.com/spf13/cobra"
)

// hookCmd is the parent "vibe hook" command.
var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Manage remote hooks (add, list, show, remove, update).",
	Long: `Register and manage remote hooks — external HTTP services exposed as LLM tools.

A remote hook points at an OpenAPI v3 service. When used in a chain the runtime
fetches its schema, discovers every operation, and makes them callable by the model.

  vibe hook add <name> --url <endpoint> [--header "Key: Value"]... [--timeout ms]
  vibe hook list
  vibe hook show <name>
  vibe hook remove <name>
  vibe hook update <name> [--url <endpoint>] [--header "Key: Value"]... [--timeout ms]`,
	SilenceUsage: true,
}

var hookAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register a remote hook by name and URL.",
	Args:  cobra.ExactArgs(1),
	RunE:  runHookAdd,
}

var hookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered remote hooks.",
	Args:  cobra.NoArgs,
	RunE:  runHookList,
}

var hookShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details and available tools for a remote hook.",
	Args:  cobra.ExactArgs(1),
	RunE:  runHookShow,
}

var hookRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a registered remote hook.",
	Args:  cobra.ExactArgs(1),
	RunE:  runHookRemove,
}

var hookUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update an existing remote hook's URL, headers, or timeout.",
	Args:  cobra.ExactArgs(1),
	RunE:  runHookUpdate,
}

func init() {
	hookAddCmd.Flags().String("url", "", "Base URL of the remote hook service (required)")
	_ = hookAddCmd.MarkFlagRequired("url")
	hookAddCmd.Flags().StringArray("header", nil, `Header to inject into every call, e.g. "Authorization: Bearer $TOKEN" (repeatable)`)
	hookAddCmd.Flags().Int("timeout", 10000, "Request timeout in milliseconds")

	hookUpdateCmd.Flags().String("url", "", "New base URL")
	hookUpdateCmd.Flags().StringArray("header", nil, `Header to inject, e.g. "Authorization: Bearer $TOKEN" (repeatable; replaces all existing headers)`)
	hookUpdateCmd.Flags().Int("timeout", 0, "New timeout in milliseconds (0 = keep existing)")

	hookCmd.AddCommand(hookAddCmd, hookListCmd, hookShowCmd, hookRemoveCmd, hookUpdateCmd)
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

// probeTools fetches the OpenAPI schema and returns the number of tools discovered.
// Returns -1 on failure (non-fatal — we warn but still register the hook).
func probeTools(endpoint string) int {
	proto := &hooks.OpenAPIToolProtocol{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := proto.FetchTools(ctx, endpoint, nil, http.DefaultClient)
	if err != nil {
		return -1
	}
	return len(tools)
}

func runHookAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	url, _ := cmd.Flags().GetString("url")
	rawHeaders, _ := cmd.Flags().GetStringArray("header")
	timeoutMs, _ := cmd.Flags().GetInt("timeout")

	headers, err := parseHeaders(rawHeaders)
	if err != nil {
		return err
	}

	ctx, db, cleanup, err := openSessionDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	store := runtimetypes.New(db.WithoutTransaction())

	// Check name not already taken.
	if _, err := store.GetRemoteHookByName(ctx, name); err == nil {
		return fmt.Errorf("hook %q already exists; use 'vibe hook update' to modify it", name)
	}

	// Probe tools (non-fatal).
	toolCount := probeTools(url)

	hook := &runtimetypes.RemoteHook{
		Name:        name,
		EndpointURL: url,
		TimeoutMs:   timeoutMs,
		Headers:     headers,
	}
	if err := store.CreateRemoteHook(ctx, hook); err != nil {
		return fmt.Errorf("failed to register hook: %w", err)
	}

	if toolCount >= 0 {
		fmt.Printf("Registered hook %q — %d tool(s) discovered.\n", name, toolCount)
	} else {
		fmt.Printf("Registered hook %q — could not reach endpoint to count tools (will retry at chain execution time).\n", name)
	}
	return nil
}

func runHookList(cmd *cobra.Command, args []string) error {
	ctx, db, cleanup, err := openSessionDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	store := runtimetypes.New(db.WithoutTransaction())

	var all []*runtimetypes.RemoteHook
	var cursor *time.Time
	for {
		page, err := store.ListRemoteHooks(ctx, cursor, 100)
		if err != nil {
			return fmt.Errorf("failed to list hooks: %w", err)
		}
		all = append(all, page...)
		if len(page) < 100 {
			break
		}
		last := page[len(page)-1].CreatedAt
		cursor = &last
	}

	if len(all) == 0 {
		fmt.Println("No remote hooks registered. Run: vibe hook add <name> --url <endpoint>")
		return nil
	}

	fmt.Printf("%-20s  %-45s  %s\n", "NAME", "URL", "TIMEOUT")
	fmt.Printf("%-20s  %-45s  %s\n", strings.Repeat("-", 20), strings.Repeat("-", 45), "-------")
	for _, h := range all {
		urlStr := h.EndpointURL
		if len(urlStr) > 45 {
			urlStr = urlStr[:42] + "..."
		}
		fmt.Printf("%-20s  %-45s  %dms\n", h.Name, urlStr, h.TimeoutMs)
	}
	return nil
}

func runHookShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx, db, cleanup, err := openSessionDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	store := runtimetypes.New(db.WithoutTransaction())
	hook, err := store.GetRemoteHookByName(ctx, name)
	if err != nil {
		return fmt.Errorf("hook %q not found", name)
	}

	fmt.Printf("Name:      %s\n", hook.Name)
	fmt.Printf("URL:       %s\n", hook.EndpointURL)
	fmt.Printf("Timeout:   %dms\n", hook.TimeoutMs)
	fmt.Printf("Registered:%s\n", hook.CreatedAt.Local().Format("2006-01-02 15:04:05"))

	if len(hook.Headers) > 0 {
		fmt.Printf("Headers:   ")
		keys := make([]string, 0, len(hook.Headers))
		for k := range hook.Headers {
			keys = append(keys, k)
		}
		fmt.Println(strings.Join(keys, ", ") + " (values hidden)")
	}

	// Probe live tools.
	proto := &hooks.OpenAPIToolProtocol{}
	toolCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Build inject params from headers for probe.
	injectParams := make(map[string]hooks.ParamArg, len(hook.Headers))
	for k, v := range hook.Headers {
		injectParams[k] = hooks.ParamArg{Name: k, Value: v, In: hooks.ArgLocationHeader}
	}

	tools, err := proto.FetchTools(toolCtx, hook.EndpointURL, injectParams, http.DefaultClient)
	if err != nil {
		fmt.Printf("Tools:     (could not reach endpoint: %v)\n", err)
		return nil
	}

	fmt.Printf("Tools (%d):\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  %-30s  %s\n", t.Function.Name, t.Function.Description)
	}
	return nil
}

func runHookRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx, db, cleanup, err := openSessionDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	store := runtimetypes.New(db.WithoutTransaction())
	hook, err := store.GetRemoteHookByName(ctx, name)
	if err != nil {
		return fmt.Errorf("hook %q not found", name)
	}
	if err := store.DeleteRemoteHook(ctx, hook.ID); err != nil {
		return fmt.Errorf("failed to remove hook: %w", err)
	}
	fmt.Printf("Removed hook %q.\n", name)
	return nil
}

func runHookUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]
	ctx, db, cleanup, err := openSessionDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	store := runtimetypes.New(db.WithoutTransaction())
	hook, err := store.GetRemoteHookByName(ctx, name)
	if err != nil {
		return fmt.Errorf("hook %q not found", name)
	}

	if cmd.Flags().Changed("url") {
		hook.EndpointURL, _ = cmd.Flags().GetString("url")
	}
	if cmd.Flags().Changed("timeout") {
		hook.TimeoutMs, _ = cmd.Flags().GetInt("timeout")
	}
	if cmd.Flags().Changed("header") {
		rawHeaders, _ := cmd.Flags().GetStringArray("header")
		headers, err := parseHeaders(rawHeaders)
		if err != nil {
			return err
		}
		hook.Headers = headers
	}

	if err := store.UpdateRemoteHook(ctx, hook); err != nil {
		return fmt.Errorf("failed to update hook: %w", err)
	}
	fmt.Printf("Updated hook %q.\n", name)
	return nil
}
