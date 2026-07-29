package contenoxcli

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/spf13/cobra"
)

var nsSuffixRe = regexp.MustCompile(`^(.+?)-[0-9a-fA-F][0-9a-fA-F-]{7,}$`)

func deriveNamespace(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	if m := nsSuffixRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

type sessionIndexRow struct {
	id        string
	identity  string
	workspace string
	name      string
	msgs      int
}

func querySessionIndex(ctx context.Context, exec libdb.Exec) ([]sessionIndexRow, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT mi.id, mi.identity, mi.workspace_id, COALESCE(mi.name, ''),
		       (SELECT COUNT(*) FROM messages m WHERE m.idx_id = mi.id)
		FROM message_indices mi`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sessionIndexRow
	for rows.Next() {
		var r sessionIndexRow
		if err := rows.Scan(&r.id, &r.identity, &r.workspace, &r.name, &r.msgs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var sessionWorkspacesCmd = &cobra.Command{
	Use:   "workspaces",
	Short: "List workspaces and session namespaces across the whole database.",
	Long: `List every workspace and the session namespaces within it, with
session and message counts, scanning the entire database (not just the
active CLI identity/workspace).

A namespace is the session-name prefix before its generated id
(e.g. jetbrainsgoland, zed, default, session).`,
	Args: cobra.NoArgs,
	RunE: runSessionWorkspaces,
}

func runSessionWorkspaces(cmd *cobra.Command, _ []string) error {
	ctx, db, _, cleanup, err := openSessionService(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := querySessionIndex(ctx, db.WithoutTransaction())
	if err != nil {
		return fmt.Errorf("failed to scan sessions: %w", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sessions in database.")
		return nil
	}

	type agg struct{ sessions, msgs int }
	groups := map[string]*agg{}
	for _, r := range rows {
		key := r.workspace + "\x00" + deriveNamespace(r.name) + "\x00" + r.identity
		g := groups[key]
		if g == nil {
			g = &agg{}
			groups[key] = g
		}
		g.sessions++
		g.msgs += r.msgs
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tNAMESPACE\tIDENTITY\tSESSIONS\tMESSAGES")
	for _, k := range keys {
		p := strings.SplitN(k, "\x00", 3)
		g := groups[k]
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n", p[0], p[1], p[2], g.sessions, g.msgs)
	}
	return w.Flush()
}

func runSessionListFiltered(cmd *cobra.Command, ctx context.Context, db libdb.DBManager, workspace, namespace string, all bool) error {
	rows, err := querySessionIndex(ctx, db.WithoutTransaction())
	if err != nil {
		return fmt.Errorf("failed to scan sessions: %w", err)
	}

	var filtered []sessionIndexRow
	for _, r := range rows {
		if !all {
			if workspace != "" && r.workspace != workspace {
				continue
			}
			if namespace != "" && deriveNamespace(r.name) != namespace {
				continue
			}
		}
		filtered = append(filtered, r)
	}
	if len(filtered) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No matching sessions.")
		return nil
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].workspace != filtered[j].workspace {
			return filtered[i].workspace < filtered[j].workspace
		}
		return filtered[i].name < filtered[j].name
	})

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tNAME\tIDENTITY\tMESSAGES\tID")
	for _, r := range filtered {
		name := r.name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", r.workspace, name, r.identity, r.msgs, r.id)
	}
	return w.Flush()
}

func resolveSessionByID(ctx context.Context, db libdb.DBManager, id string) (name string, found bool) {
	row := db.WithoutTransaction().QueryRowContext(ctx,
		`SELECT COALESCE(name, '') FROM message_indices WHERE id = $1`, id)
	if err := row.Scan(&name); err != nil {
		return "", false
	}
	return name, true
}
