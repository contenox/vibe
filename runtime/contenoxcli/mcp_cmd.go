package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/runtime/localhooks"
	"github.com/contenox/contenox/runtime/mcpserverservice"
	"github.com/contenox/contenox/runtime/runtimetypes"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP servers (add, list, show, remove).",
	Long: `Register and manage Model Context Protocol (MCP) servers.

MCP servers extend the runtime with additional tools and resources callable by the model.
Two transport modes are supported:

  stdio   Spawn a local process and communicate via stdin/stdout.
          Requires --command (and optionally --args).

  sse     Connect to a remote MCP server via Server-Sent Events.
          Requires --url.

  http    Connect to a remote MCP server via HTTP streaming.
          Requires --url.

Examples:
  # Register a local stdio MCP server:
  contenox-runtime mcp add myserver --transport stdio --command npx --args "-y,@modelcontextprotocol/server-filesystem,/tmp"

  # Register a remote SSE-based MCP server:
  contenox-runtime mcp add remote --transport sse --url https://mcp.example.com/sse

  contenox-runtime mcp list
  contenox-runtime mcp show myserver
  contenox-runtime mcp remove myserver`,
}

var mcpAddCmd = &cobra.Command{
	Use:   "add <name> [url]",
	Short: "Register an MCP server.",
	Long: `Register a named MCP server in the local SQLite database.

Transport types:
  stdio   Spawn a local command (--command required; --args optional)
  sse     Connect to a remote server via Server-Sent Events (--url required)
  http    Connect to a remote server via HTTP streaming (--url required)

You can pass the URL directly as a second positional argument. The transport
defaults to "http" when a URL is provided this way.

For authentication, use:
  --auth-type bearer  with --auth-token <token>  or  --auth-env <ENV_VAR>

Examples:
  # Shorthand: name + URL (transport defaults to http)
  contenox-runtime mcp add notion https://mcp.notion.com/mcp

  # Stdio: spawn a local filesystem MCP server
  contenox-runtime mcp add fs --transport stdio \
    --command npx --args "-y,@modelcontextprotocol/server-filesystem,/tmp"

  # SSE: connect to a remote MCP endpoint
  contenox-runtime mcp add remote --transport sse --url https://mcp.example.com/sse \
    --auth-type bearer --auth-env MCP_TOKEN`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		flags := cmd.Flags()

		transport, _ := flags.GetString("transport")
		command, _ := flags.GetString("command")
		cmdArgs, _ := flags.GetStringSlice("args")
		url, _ := flags.GetString("url")
		timeout, _ := flags.GetInt("timeout")

		authType, _ := flags.GetString("auth-type")
		authToken, _ := flags.GetString("auth-token")
		authEnv, _ := flags.GetString("auth-env")
		rawHeaders, _ := flags.GetStringArray("header")
		rawInjects, _ := flags.GetStringArray("inject")

		// Positional shorthand: contenox-runtime mcp add <name> <url>
		// If a second positional arg is provided, treat it as --url and default
		// transport to "http" unless the user explicitly set --transport.
		if len(args) == 2 {
			if url == "" {
				url = args[1]
			}
			if !flags.Changed("transport") {
				transport = "http"
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

		if transport == "" {
			return fmt.Errorf(
				"--transport is required (stdio, sse, http)\n\n"+
					"  For a remote HTTP server:\n"+
					"    contenox-runtime mcp add %s https://<url>\n"+
					"  For a local stdio server:\n"+
					"    contenox-runtime mcp add %s --transport stdio --command <cmd>",
				name, name,
			)
		}
		if transport == "stdio" && command == "" {
			return fmt.Errorf(
				"--command is required for stdio transport\n\n"+
					"  Example:\n"+
					"    contenox-runtime mcp add %s --transport stdio --command npx --args \"-y,@modelcontextprotocol/server-filesystem,/tmp\"",
				name,
			)
		}
		if (transport == "sse" || transport == "http") && url == "" {
			return fmt.Errorf(
				"--url is required for %s transport\n\n"+
					"  Example:\n"+
					"    contenox-runtime mcp add %s --transport %s --url https://<host>/mcp\n"+
					"  Or use the shorthand:\n"+
					"    contenox-runtime mcp add %s https://<host>/mcp",
				transport, name, transport, name,
			)
		}

		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		srv := &runtimetypes.MCPServer{
			Name:                  name,
			Transport:             transport,
			Command:               command,
			Args:                  cmdArgs,
			URL:                   url,
			ConnectTimeoutSeconds: timeout,
			AuthType:              authType,
			AuthToken:             authToken,
			AuthEnvKey:            authEnv,
			Headers:               headers,
			InjectParams:          injectParams,
		}

		if err := svc.Create(ctx, srv); err != nil {
			return fmt.Errorf("failed to add MCP server: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "MCP server %q added successfully.\n", name)
		return nil
	},
}

var mcpListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered MCP servers.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		servers, err := svc.List(ctx, nil, 100)
		if err != nil {
			return fmt.Errorf("failed to list MCP servers: %w", err)
		}

		if len(servers) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No MCP servers registered. Run: contenox-runtime mcp add <name> --transport <type> ...")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTRANSPORT\tCOMMAND/URL")
		for _, s := range servers {
			target := s.Command
			if s.Transport == "sse" || s.Transport == "http" {
				target = s.URL
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, s.Transport, target)
		}
		return w.Flush()
	},
}

var mcpShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details for an MCP server.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		srv, err := svc.GetByName(ctx, name)
		if err != nil {
			return fmt.Errorf("mcp server %q not found: %w", name, err)
		}

		// Never print auth token values.
		display := *srv
		if display.AuthToken != "" {
			display.AuthToken = "(set, value hidden)"
		}
		if len(display.Headers) > 0 {
			hidden := make(map[string]string, len(display.Headers))
			for k := range display.Headers {
				hidden[k] = "(hidden)"
			}
			display.Headers = hidden
		}
		if len(display.InjectParams) > 0 {
			hidden := make(map[string]string, len(display.InjectParams))
			for k := range display.InjectParams {
				hidden[k] = "(hidden)"
			}
			display.InjectParams = hidden
		}

		b, _ := json.MarshalIndent(display, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	},
}

var mcpRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a registered MCP server.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		srv, err := svc.GetByName(ctx, name)
		if err != nil {
			return fmt.Errorf("mcp server %q not found: %w", name, err)
		}

		if err := svc.Delete(ctx, srv.ID); err != nil {
			return fmt.Errorf("failed to remove mcp server: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "MCP server %q removed.\n", name)
		return nil
	},
}

func openMCPService(cmd *cobra.Command) (libdb.DBManager, mcpserverservice.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	return db, mcpserverservice.New(db), nil
}

var mcpUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update an existing MCP server registration.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]
		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		srv, err := svc.GetByName(ctx, name)
		if err != nil {
			return fmt.Errorf("mcp server %q not found: %w", name, err)
		}

		flags := cmd.Flags()
		if flags.Changed("timeout") {
			srv.ConnectTimeoutSeconds, _ = flags.GetInt("timeout")
		}
		if flags.Changed("auth-type") {
			srv.AuthType, _ = flags.GetString("auth-type")
		}
		if flags.Changed("auth-token") {
			srv.AuthToken, _ = flags.GetString("auth-token")
		}
		if flags.Changed("auth-env") {
			srv.AuthEnvKey, _ = flags.GetString("auth-env")
		}
		if flags.Changed("header") {
			rawHeaders, _ := flags.GetStringArray("header")
			headers, err := parseHeaders(rawHeaders)
			if err != nil {
				return err
			}
			srv.Headers = headers
		}
		if flags.Changed("inject") {
			rawInjects, _ := flags.GetStringArray("inject")
			injectParams, err := parseInjects(rawInjects)
			if err != nil {
				return err
			}
			srv.InjectParams = injectParams
		}

		if err := svc.Update(ctx, srv); err != nil {
			return fmt.Errorf("failed to update mcp server: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "MCP server %q updated.\n", name)
		return nil
	},
}

var mcpAuthCmd = &cobra.Command{
	Use:   "auth <name>",
	Short: "Authenticate an OAuth MCP server (opens browser).",
	Long: `Run the OAuth 2.1 PKCE authorization flow for a registered MCP server.

Opens your browser at the server's authorization page.
After you approve access, the token is stored locally and used for all
subsequent connections — no re-authentication needed until it expires.

Example:
  contenox-runtime mcp auth notion`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		name := args[0]

		db, svc, err := openMCPService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		if err := svc.AuthenticateOAuth(ctx, name, &localhooks.MCPOAuthConfig{}); err != nil {
			return fmt.Errorf("mcp oauth auth: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: authenticated successfully.\n", name)
		return nil
	},
}

func init() {
	mcpAddCmd.Flags().String("transport", "", "Transport type: stdio (local process), sse, or http (remote server)")
	mcpAddCmd.Flags().String("command", "", "Command to execute (required for stdio transport)")
	mcpAddCmd.Flags().StringSlice("args", nil, "Arguments for the command, comma-separated (for stdio transport)")
	mcpAddCmd.Flags().String("url", "", "URL of the remote MCP server (required for sse/http transport)")
	mcpAddCmd.Flags().Int("timeout", 0, "Connection timeout in seconds (0 = no timeout)")

	mcpAddCmd.Flags().String("auth-type", "", "Authentication type: bearer or oauth")
	mcpAddCmd.Flags().String("auth-token", "", "Authentication token literal (prefer --auth-env)")
	mcpAddCmd.Flags().String("auth-env", "", "Environment variable containing the authentication token")
	mcpAddCmd.Flags().StringArray("header", nil, `Additional HTTP header for SSE/HTTP transport, e.g. "X-Tenant: acme" (repeatable)`)
	mcpAddCmd.Flags().StringArray("inject", nil, `Tool call param to inject and hide from model, e.g. "tenant_id=acme" (repeatable)`)

	mcpUpdateCmd.Flags().Int("timeout", 0, "New connection timeout in seconds")
	mcpUpdateCmd.Flags().String("auth-type", "", "New authentication type")
	mcpUpdateCmd.Flags().String("auth-token", "", "New authentication token literal")
	mcpUpdateCmd.Flags().String("auth-env", "", "New environment variable for auth token")
	mcpUpdateCmd.Flags().StringArray("header", nil, `Additional HTTP headers (replaces all existing headers)`)
	mcpUpdateCmd.Flags().StringArray("inject", nil, `Injected tool call params (replaces all existing inject params)`)

	mcpCmd.AddCommand(mcpAddCmd)
	mcpCmd.AddCommand(mcpListCmd)
	mcpCmd.AddCommand(mcpShowCmd)
	mcpCmd.AddCommand(mcpRemoveCmd)
	mcpCmd.AddCommand(mcpUpdateCmd)
	mcpCmd.AddCommand(mcpAuthCmd)
}
