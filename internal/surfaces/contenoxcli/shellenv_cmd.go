package contenoxcli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/contenox/contenox/internal/services/shellenvservice"
	"github.com/spf13/cobra"
)

var shellEnvCmd = &cobra.Command{
	Use:   "shell-env",
	Short: "Manage the environment variables contenox injects into the shells it spawns.",
	Long: `Manage the global environment variables contenox INJECTS into the shells it
spawns — the local_shell tool, the shell_session PTY, and the interactive
terminal. They are added on top of whatever the environment scrub passes through,
so an injected value always wins, and they apply even when a scrub mode is off.

Scope is global (every spawned shell). Values are stored as plain configuration,
editable only through this command, and read live — an edit applies to the
next shell that spawns. Do not store secrets here.

  contenox shell-env set HTTP_PROXY=http://proxy:3128 GOCACHE=/var/cache/go
  contenox shell-env list
  contenox shell-env unset HTTP_PROXY

To see what the scrub itself keeps or strips, use 'contenox sandbox env'.`,
}

var shellEnvSetCmd = &cobra.Command{
	Use:   "set KEY=VALUE [KEY=VALUE ...]",
	Short: "Set (add or override) one or more injected variables.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runShellEnvSet,
}

var shellEnvUnsetCmd = &cobra.Command{
	Use:   "unset KEY [KEY ...]",
	Short: "Remove one or more injected variables.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runShellEnvUnset,
}

var shellEnvListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the injected variables.",
	Args:  cobra.NoArgs,
	RunE:  runShellEnvList,
}

func init() {
	shellEnvCmd.AddCommand(shellEnvSetCmd)
	shellEnvCmd.AddCommand(shellEnvUnsetCmd)
	shellEnvCmd.AddCommand(shellEnvListCmd)
}

func runShellEnvSet(cmd *cobra.Command, args []string) error {
	pairs := make(map[string]string, len(args))
	for _, a := range args {
		eq := strings.IndexByte(a, '=')
		if eq <= 0 {
			return fmt.Errorf("shell-env set: %q is not KEY=VALUE", a)
		}
		name := a[:eq]
		if !shellenvservice.ValidEnvName(name) {
			return fmt.Errorf("shell-env set: %q is not a valid variable name (letters, digits, underscores; not starting with a digit)", name)
		}
		pairs[name] = a[eq+1:]
	}
	return withShellEnv(cmd, func(ctx context.Context, svc shellenvservice.Service) error {
		vars, err := svc.Get(ctx)
		if err != nil {
			return err
		}
		for k, v := range pairs {
			vars[k] = v
		}
		if err := svc.Set(ctx, vars); err != nil {
			return err
		}
		printShellEnv(cmd, vars)
		return nil
	})
}

func runShellEnvUnset(cmd *cobra.Command, args []string) error {
	return withShellEnv(cmd, func(ctx context.Context, svc shellenvservice.Service) error {
		vars, err := svc.Get(ctx)
		if err != nil {
			return err
		}
		for _, k := range args {
			delete(vars, k)
		}
		if err := svc.Set(ctx, vars); err != nil {
			return err
		}
		printShellEnv(cmd, vars)
		return nil
	})
}

func runShellEnvList(cmd *cobra.Command, _ []string) error {
	return withShellEnv(cmd, func(ctx context.Context, svc shellenvservice.Service) error {
		vars, err := svc.Get(ctx)
		if err != nil {
			return err
		}
		printShellEnv(cmd, vars)
		return nil
	})
}

// withShellEnv opens the database, builds the shell-env service, runs fn, and
// closes the database.
func withShellEnv(cmd *cobra.Command, fn func(context.Context, shellenvservice.Service) error) error {
	db, _, err := openConfigDB(cmd)
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(context.Background(), shellenvservice.New(db))
}

func printShellEnv(cmd *cobra.Command, vars map[string]string) {
	out := cmd.OutOrStdout()
	if len(vars) == 0 {
		fmt.Fprintln(out, "# no global shell-env variables set")
		return
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "%s=%s\n", name, vars[name])
	}
}
