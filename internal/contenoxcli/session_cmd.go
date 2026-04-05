// session_cmd.go — contenox session subcommand tree (new, list, switch, delete, show).
// Each subcommand opens only the DB via sessionservice; no LLM stack is needed.
package contenoxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/messagestore"
	"github.com/contenox/contenox/sessionservice"
	"github.com/contenox/contenox/taskengine"
	"github.com/spf13/cobra"
)

// sessionCmd is the parent "contenox session" command.
var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Manage chat sessions (new, list, switch, delete, show).",
	Long: `Create and switch named chat sessions.
Each session maintains its own persistent conversation history.

  contenox session new [name]     create a session and make it active
  contenox session list           list all sessions (* = active)
  contenox session switch <name>  switch the active session
  contenox session delete <name>  delete a session and its messages
  contenox session show           print the active session's conversation`,
	SilenceUsage: true,
}

var sessionNewCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Create a new session and make it active.",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runSessionNew,
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all sessions (* = active).",
	Args:  cobra.NoArgs,
	RunE:  runSessionList,
}

var sessionSwitchCmd = &cobra.Command{
	Use:   "switch <name>",
	Short: "Switch the active session by name.",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionSwitch,
}

var sessionDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a session and all its messages.",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionDelete,
}

var sessionShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Print a session's conversation (default: active session).",
	Long: `Print the full conversation history for a session.

Defaults to the currently active session. Pass a session name to view another.

Flags:
  --tail N    Show only the last N messages
  --head N    Show only the first N messages

Examples:
  contenox session show
  contenox session show my-session
  contenox session show --tail 10`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionShow,
}

func init() {
	sessionShowCmd.Flags().Int("tail", 0, "Show last N messages (0 = all)")
	sessionShowCmd.Flags().Int("head", 0, "Show first N messages (0 = all)")
	sessionCmd.AddCommand(sessionNewCmd, sessionListCmd, sessionSwitchCmd, sessionDeleteCmd, sessionShowCmd)
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
	cleanup := func() { _ = db.Close() }
	return ctx, db, sessionservice.New(db), cleanup, nil
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
	ctx, _, svc, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

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
		// Resolve name → ID via raw messagestore (read-only, presentation path).
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
			return fmt.Errorf("session %q not found; run 'contenox session list'", args[0])
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

	// Raw message read for rendering — remains direct (presentation, not CRUD).
	store := messagestore.New(db.WithoutTransaction())
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
