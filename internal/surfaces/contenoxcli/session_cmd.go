// session_cmd.go — contenox session subcommand tree (new, list, switch, delete, show).
// Each subcommand opens only the DB via sessionservice; no LLM stack is needed.
package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/libtracker"
	"github.com/contenox/contenox/internal/services/messagestore"
	"github.com/contenox/contenox/internal/services/sessionservice"
	"github.com/spf13/cobra"
)

// sessionCmd is the parent "contenox session" command.
var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage chat sessions (new, list, switch, delete, show, fork, workspaces).",
	Long: `Create and switch named chat sessions.
Each session maintains its own persistent conversation history.

  contenox session new [name]            create a session and make it active
  contenox session list                  list active-scope sessions (* = active)
  contenox session list --workspace W    list sessions in a workspace (whole DB)
  contenox session list --namespace NS   list sessions in a namespace (whole DB)
  contenox session list --all            list every session (whole DB)
  contenox session switch <name>         switch the active session
  contenox session delete <name>         delete a session and its messages
  contenox session show [name|id]        print a session (active, by name, or id)
  contenox session fork [name]           copy active session to a new one
  contenox session fork --summary        compact older history before forking
  contenox session workspaces            list workspaces and namespaces (whole DB)`,
	SilenceUsage: true,
}

var sessionNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Create a new session and make it active.",
	Long: `Create a new chat session and immediately make it the active one.
An optional name may be given; when omitted, the session is identified by a
short prefix of its generated id.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionNew,
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions: active-scope by default, or whole-DB with --workspace/--namespace/--all.",
	Long: `List chat sessions with their message counts, marking the active one with '*'.
By default only sessions in the active scope are shown. Pass --workspace,
--namespace, or --all to scan the whole database for sessions in that
workspace, namespace, or everywhere.`,
	Args: cobra.NoArgs,
	RunE: runSessionList,
}

var sessionSwitchCmd = &cobra.Command{
	Use:   "switch <name>",
	Short: "Switch the active session by name.",
	Long: `Make the named session the active one, so subsequent chat and run commands
use its conversation history. Run 'contenox session list' to see the names
available to switch to.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionSwitch,
}

var sessionDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a session and all its messages.",
	Long: `Permanently delete the named session and its entire message history.
If the deleted session was the active one, no session is active afterward;
create or switch to another before chatting again.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionDelete,
}

var sessionShowCmd = &cobra.Command{
	Use:   "show [name|id]",
	Short: "Print a session's conversation (active by default; by name or session id).",
	Long: `Print the full conversation history for a session.

Defaults to the active session. Pass a session name (active scope) or a
session id, which is resolved across any workspace/identity. Use
'contenox session list --all' or 'contenox session workspaces' to find ids.

Flags:
  --tail N    Show only the last N messages
  --head N    Show only the first N messages
  --ns NS     Namespace hint when resolving by id (advisory)

Examples:
  contenox session show
  contenox session show my-session
  contenox session show 6644fc61-fbc6-4e55-9006-b46178625de3
  contenox session show --tail 10`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionShow,
}

func init() {
	sessionShowCmd.Flags().Int("tail", 0, "Show last N messages (0 = all)")
	sessionShowCmd.Flags().Int("head", 0, "Show first N messages (0 = all)")
	sessionShowCmd.Flags().String("ns", "", "Namespace hint when resolving by id (advisory)")
	sessionListCmd.Flags().String("workspace", "", "List sessions in this workspace id (scans whole DB)")
	sessionListCmd.Flags().String("namespace", "", "List sessions in this namespace (scans whole DB)")
	sessionListCmd.Flags().Bool("all", false, "List every session across all workspaces and namespaces")
	sessionCmd.AddCommand(sessionNewCmd, sessionListCmd, sessionSwitchCmd, sessionDeleteCmd, sessionShowCmd, sessionWorkspacesCmd)
}

// openSessionService resolves the DB path and returns a sessionservice.Service.
func openSessionService(cmd *cobra.Command) (context.Context, libdb.DBManager, sessionservice.Service, func(), error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	contenoxDir, _ := ResolveContenoxDir(cmd)
	workspaceID := ResolveWorkspaceID(contenoxDir)
	cleanup := func() { _ = db.Close() }
	return ctx, db, sessionservice.New(db, workspaceID, libtracker.NoopTracker{}), cleanup, nil
}

func runSessionNew(cmd *cobra.Command, args []string) error {
	ctx, _, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	id, err := svc.New(ctx, localIdentity, name)
	if err != nil {
		return err
	}
	if name == "" {
		name = id[:8] + "…"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created session %q. Now active.\n", name)
	return nil
}

func runSessionList(cmd *cobra.Command, _ []string) error {
	ctx, db, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ws, _ := cmd.Flags().GetString("workspace")
	ns, _ := cmd.Flags().GetString("namespace")
	all, _ := cmd.Flags().GetBool("all")
	if ws != "" || ns != "" || all {
		return runSessionListFiltered(cmd, ctx, db, ws, ns, all)
	}

	sessions, err := svc.List(ctx, localIdentity)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions yet. Run: contenox session new")
		return nil
	}
	for _, s := range sessions {
		prefix := "  "
		if s.IsActive {
			prefix = "* "
		}
		displayName := s.Name
		if displayName == "" {
			displayName = s.ID[:8] + "…"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s%-24s (%d messages)\n", prefix, displayName, s.MessageCount)
	}
	return nil
}

func runSessionSwitch(cmd *cobra.Command, args []string) error {
	ctx, _, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	name := args[0]
	if err := svc.Switch(ctx, localIdentity, name); err != nil {
		return fmt.Errorf("%w; run 'contenox session list' to see available sessions", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Switched to session %q.\n", name)
	return nil
}

func runSessionDelete(cmd *cobra.Command, args []string) error {
	ctx, _, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	name := args[0]
	wasActive, err := svc.Delete(ctx, localIdentity, name)
	if err != nil {
		return err
	}
	if wasActive {
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted session %q (was active; run 'contenox session new' or 'contenox session switch' to set a new active session).\n", name)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted session %q.\n", name)
	}
	return nil
}

func runSessionShow(cmd *cobra.Command, args []string) error {
	ctx, db, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	tailN, _ := cmd.Flags().GetInt("tail")
	headN, _ := cmd.Flags().GetInt("head")

	// Resolve which session to show.
	var sessionID, sessionName string
	if len(args) > 0 {
		if nm, ok := resolveSessionByID(ctx, db, args[0]); ok {
			sessionID = args[0]
			sessionName = nm
			if sessionName == "" {
				sessionName = sessionID[:8] + "…"
			}
		} else {
			sessions, err := svc.List(ctx, localIdentity)
			if err != nil {
				return err
			}
			for _, s := range sessions {
				if s.Name == args[0] {
					sessionID = s.ID
					sessionName = s.Name
					break
				}
			}
			if sessionID == "" {
				return fmt.Errorf("session %q not found; run 'contenox session list' or 'contenox session workspaces'", args[0])
			}
		}
	} else {
		activeID, err := svc.GetActiveID(ctx)
		if err != nil || activeID == "" {
			return fmt.Errorf("no active session; run 'contenox session new' to create one")
		}
		sessionID = activeID
		sessionName = sessionID[:8] + "…"
		sessions, _ := svc.List(ctx, localIdentity)
		for _, s := range sessions {
			if s.ID == sessionID && s.Name != "" {
				sessionName = s.Name
				break
			}
		}
	}

	contenoxDir, _ := ResolveContenoxDir(cmd)
	store := messagestore.New(db.WithoutTransaction(), ResolveWorkspaceID(contenoxDir))
	rawMsgs, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to read messages: %w", err)
	}
	if len(rawMsgs) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Session %q has no messages yet.\n", sessionName)
		return nil
	}

	// Apply head/tail filters.
	slice := rawMsgs
	if headN > 0 && headN < len(slice) {
		slice = slice[:headN]
	} else if tailN > 0 && tailN < len(slice) {
		slice = slice[len(slice)-tailN:]
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "━━━━ Session: %s (%d/%d messages) ━━━━\n", sessionName, len(slice), len(rawMsgs))
	for _, raw := range slice {
		var m taskengine.Message
		if err := json.Unmarshal(raw.Payload, &m); err != nil {
			continue
		}
		ts := ""
		if !m.Timestamp.IsZero() {
			ts = m.Timestamp.Format(time.RFC3339)
		}
		if ts != "" {
			fmt.Fprintf(out, "[%s] %s:\n", ts, m.Role)
		} else {
			fmt.Fprintf(out, "%s:\n", m.Role)
		}
		fmt.Fprintf(out, "  %s\n\n", m.Content)
	}
	fmt.Fprintf(out, "━━━━━━━━━━━━━━━━━━━━\n")
	return nil
}
