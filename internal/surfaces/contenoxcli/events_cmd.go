package contenoxcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/vfs"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Tail the durable event log and dispatch triggers (beta).",
	Long: `Operate the durable event-dispatch tier (opt-in-beta).

Domain events (mission reports, status changes, plan revisions, attention
asks) are appended to a durable log in local.db. Operator-authored
trigger-*.json files bind an event type to a task chain; the dispatcher fires
the chain with the event JSON as input.

Trigger files are discovered like every system file: workspace .contenox/
first, ~/.contenox as fallback. Shape:

  {
    "name": "on-report",
    "description": "summarise every mission report",
    "listen_for": {"type": "missionservice.events.report_added"},
    "type": "fire_chain",
    "chain": "chain-on-report.json",
    "policy": "hitl-policy-default.json"
  }

Validate trigger files with 'contenox vet'; 'contenox doctor' lists what is
loaded.`,
}

var eventsDispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Run the trigger dispatcher in the foreground.",
	Long: `Run the event dispatcher: catch up on events appended while no dispatcher
was running (a durable cursor marks where the last one stopped), then follow
new events live. One line is printed per firing; a chain failure is recorded
on the firing and never stops the loop. Stop with Ctrl-C.

Each (trigger, event) pair fires at most once — restarts and the
live/catch-up overlap dedup through a durable firings table. Events carry a
hop count; a chain fired by the dispatcher stamps hop+1 on events it causes,
and events past hop 4 are refused, so triggers cannot loop forever.`,
	Args: cobra.NoArgs,
	RunE: runEventsDispatch,
}

var eventsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List events from the durable log.",
	Long: `List events from the durable event log in append (NID) order.

  contenox events list                 the first events in the log
  contenox events list --since 41     events with nid > 41`,
	Args: cobra.NoArgs,
	RunE: runEventsList,
}

var eventsFiringsCmd = &cobra.Command{
	Use:   "firings",
	Short: "List recorded trigger firings — what dispatched, failed, or was refused.",
	Long: `List the durable firing records, newest first: not what was appended to the
log, but what the dispatcher (or an engine-running host) actually did with it.

  contenox events firings                       the most recent firings
  contenox events firings --status error        chains that failed
  contenox events firings --status refused      hop-limit refusals
  contenox events firings --trigger on-report   one trigger's history
  contenox events firings --since 41            firings for events with nid > 41

A trigger whose listen_for never matches records nothing at all: an empty
listing for a trigger 'contenox doctor' says is loaded is the typo's symptom —
compare the type against 'contenox events list'.

Reading firings never appends an event and never fires a trigger.`,
	Args: cobra.NoArgs,
	RunE: runEventsFirings,
}

var eventsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Drop event-log partitions older than a retention window.",
	Long: `Drop whole per-day event-log partitions older than --keep-days. Retention
is an O(1) table drop per day — no row deletes, no VACUUM. Never automatic:
pruning runs only when you invoke it. The dispatch cursor and the firings
record are untouched.`,
	Args: cobra.NoArgs,
	RunE: runEventsPrune,
}

func init() {
	eventsDispatchCmd.Flags().Bool("auto", false, "Non-interactive mode: disable HITL approval prompts; fired chains route through the trigger's policy (or the default) without a terminal ask.")
	eventsListCmd.Flags().Int64("since", 0, "List events with nid greater than this cursor")
	eventsListCmd.Flags().Int("limit", 50, "Maximum events to list")
	eventsFiringsCmd.Flags().Int64("since", 0, "List firings for events with nid greater than this cursor")
	eventsFiringsCmd.Flags().String("status", "", "Filter by outcome: ok, error, refused, or running")
	eventsFiringsCmd.Flags().String("trigger", "", "Filter by trigger name")
	eventsFiringsCmd.Flags().Int("limit", 50, "Maximum firings to list")
	eventsPruneCmd.Flags().Int("keep-days", 30, "Keep partitions from the last N days; older ones are dropped")
	eventsPruneCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
	eventsCmd.AddCommand(eventsDispatchCmd, eventsListCmd, eventsFiringsCmd, eventsPruneCmd)
}

func triggerRoots(contenoxDir string) []string {
	roots := []string{contenoxDir}
	if homeDir, err := globalContenoxDir(); err == nil {
		roots = append(roots, homeDir)
	}
	return roots
}

func loadTriggersKept(ctx context.Context, tracker libtracker.ActivityTracker, contenoxDir string, optInBeta bool) (eventtrigger.LoadResult, error) {
	var keep func(string) bool
	if !optInBeta {
		keep = func(string) bool { return false }
	}
	return eventtrigger.LoadKept(ctx, tracker, keep, triggerRoots(contenoxDir)...)
}

func missionEventPublisher(ctx context.Context, db libdbexec.DBManager, bus missionservice.EventPublisher, workspaceID string, tracker libtracker.ActivityTracker, trigger eventlog.Trigger) missionservice.EventPublisher {
	if !betaEnabled(ctx, runtimetypes.New(db.WithoutTransaction())) {
		return bus
	}
	opts := []eventlog.DualPublisherOption{eventlog.WithSubjectField("missionId")}
	if trigger != nil {
		opts = append(opts, eventlog.WithPublisherTrigger(trigger))
	}
	return eventlog.NewDualPublisher(runtimetypes.NewEventStore(db), bus, "missionservice", workspaceID, tracker, opts...)
}

func buildInProcessTriggerHook(ctx context.Context, db libdbexec.DBManager, contenoxDir, workspaceID string, engine *Engine, opts chatOpts, warnW io.Writer) eventlog.Trigger {
	if engine == nil || !opts.EffectiveOptInBeta {
		return nil
	}
	res, err := loadTriggersKept(ctx, engine.Tracker, contenoxDir, opts.EffectiveOptInBeta)
	if err != nil || len(res.Triggers) == 0 {
		return nil
	}
	firingStore, err := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), workspaceID)
	if err != nil {
		return nil
	}
	handler, err := eventtrigger.NewHandler(eventtrigger.Deps{
		Store:       firingStore,
		WorkspaceID: workspaceID,
		Triggers:    res.Triggers,
		Runner: &chainFiringRunner{
			agent: agentservice.New(agentservice.Deps{
				Engine:      engine,
				DB:          db,
				WorkspaceID: workspaceID,
			}),
			opts:        opts,
			contenoxDir: contenoxDir,
		},
		Tracker: engine.Tracker,
	})
	if err != nil {
		if warnW != nil {
			fmt.Fprintf(warnW, "warning: in-process event dispatch unavailable: %v (the standalone `contenox events dispatch` still catches up)\n", err)
		}
		return nil
	}
	if warnW != nil {
		fmt.Fprintf(warnW, "in-process event dispatch: %d trigger(s) fire live in this process; `contenox events dispatch` remains the catch-up consumer.\n", len(res.Triggers))
	}
	return handler
}

func runEventsDispatch(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	o, err := buildRunOpts(cmd, db, contenoxDir)
	if err != nil {
		return err
	}
	o.EffectiveDB = dbPath
	// Shell defaults on: trigger chains actuate through local_shell.
	if !cmd.Root().Flags().Changed("shell") {
		o.EffectiveEnableLocalExec = true
	}

	engine, err := BuildEngine(ctx, db, o)
	if err != nil {
		return fmt.Errorf("failed to build engine: %w", err)
	}
	defer engine.Stop()

	res, err := loadTriggersKept(dbCtx, engine.Tracker, contenoxDir, o.EffectiveOptInBeta)
	if err != nil {
		return err
	}
	errOut := cmd.ErrOrStderr()
	for _, s := range res.Skipped {
		fmt.Fprintf(errOut, "warning: trigger file skipped: %s (%s)\n", s.Path, s.Reason)
	}
	if !o.EffectiveOptInBeta {
		fmt.Fprintln(errOut, "opt-in-beta is off: no triggers are loaded (contenox config set opt-in-beta true)")
	}
	fmt.Fprintf(errOut, "Loaded %d trigger(s):\n", len(res.Triggers))
	for _, t := range res.Triggers {
		fmt.Fprintf(errOut, "  %s\n", formatTriggerLine(t))
	}

	// The dispatcher is workspace-bound, under a per-workspace cursor.
	workspaceID := ResolveWorkspaceID(contenoxDir)
	fmt.Fprintf(errOut, "Workspace: %s\n", workspaceID)
	logSvc := eventlog.NewService(db, engine.Bus, engine.Tracker)
	runner := &chainFiringRunner{
		agent: agentservice.New(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: workspaceID,
		}),
		opts:        o,
		contenoxDir: contenoxDir,
	}
	out := cmd.OutOrStdout()
	firingStore, err := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), workspaceID)
	if err != nil {
		return err
	}
	dispatcher, err := eventtrigger.New(eventtrigger.Deps{
		Log:         logSvc,
		Store:       firingStore,
		WorkspaceID: workspaceID,
		Triggers:    res.Triggers,
		Runner:      runner,
		Tracker:     engine.Tracker,
		OnFiring: func(t eventtrigger.Trigger, ev runtimetypes.Event, status, requestID string, firingErr error) {
			line := fmt.Sprintf("fired %s ← %s nid=%d hop=%d status=%s request=%s", t.Name, ev.Type, ev.NID, ev.Hop, status, requestID)
			if firingErr != nil {
				line += " error=" + firingErr.Error()
			}
			fmt.Fprintln(out, line)
		},
	})
	if err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fmt.Fprintln(errOut, "Dispatching (catch-up, then live). Ctrl-C to stop.")
	if err := dispatcher.Run(runCtx); err != nil {
		return err
	}
	fmt.Fprintln(errOut, "Dispatcher stopped.")
	return nil
}

type chainFiringRunner struct {
	agent       agentservice.Agent
	opts        chatOpts
	contenoxDir string
}

func (r *chainFiringRunner) RunChain(ctx context.Context, t eventtrigger.Trigger, ev runtimetypes.Event) error {
	path, err := lookupSystemFile(r.contenoxDir, t.Chain)
	if err != nil {
		return err
	}
	chain, err := loadChainFromFile(path)
	if err != nil {
		return err
	}
	// Nobody is attached to a firing: a gated call records its ask and suspends
	// rather than holding this goroutine, its session and its model context for
	// the whole approval wait — which, unanswered, would be the full ceiling,
	// multiplied by every event in a storm. The answer arrives later through the
	// resume hook.
	execCtx := taskengine.WithDetachedAsks(ctx)
	if t.Policy != "" {
		// The trigger's named envelope; unset, hitlservice's standard resolution
		// applies.
		execCtx = hitlservice.WithPolicyName(execCtx, t.Policy)
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		execCtx = vfs.WithSessionCwd(execCtx, cwd)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	_, err = r.agent.Prompt(execCtx, agentservice.PromptRequest{
		Input:         string(raw),
		InputValue:    input,
		InputType:     taskengine.DataTypeJSON,
		Chain:         chain,
		ChainRef:      path,
		TemplateVars:  buildTemplateVars(r.opts),
		ContextLength: r.opts.EffectiveContext,
	})
	return err
}

func runEventsList(cmd *cobra.Command, args []string) error {
	since, _ := cmd.Flags().GetInt64("since")
	limit, _ := cmd.Flags().GetInt("limit")

	db, _, workspaceID, err := openConfigDBWithWorkspace(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	events, err := runtimetypes.NewEventStore(db).ListEventsSince(ctx, workspaceID, since, limit)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no events)")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NID\tTYPE\tSOURCE\tSUBJECT\tHOP\tTIME\tDATA")
	for _, e := range events {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%s\n",
			e.NID, e.Type, e.Source, e.Subject, e.Hop,
			e.Time.UTC().Format("2006-01-02T15:04:05Z"), compactEventData(e.Data, 80))
	}
	return w.Flush()
}

var firingStatuses = []string{
	runtimetypes.EventFiringStatusOK,
	runtimetypes.EventFiringStatusError,
	runtimetypes.EventFiringStatusRefused,
	runtimetypes.EventFiringStatusRunning,
}

func runEventsFirings(cmd *cobra.Command, args []string) error {
	since, _ := cmd.Flags().GetInt64("since")
	status, _ := cmd.Flags().GetString("status")
	triggerName, _ := cmd.Flags().GetString("trigger")
	limit, _ := cmd.Flags().GetInt("limit")

	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" && !slices.Contains(firingStatuses, status) {
		return fmt.Errorf("unknown --status %q: use one of %s", status, strings.Join(firingStatuses, ", "))
	}

	db, _, workspaceID, err := openConfigDBWithWorkspace(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	firingStore, err := runtimetypes.NewEventFiringStore(db.WithoutTransaction(), workspaceID)
	if err != nil {
		return err
	}
	firings, err := firingStore.ListEventFirings(ctx, runtimetypes.EventFiringFilter{
		SinceNID:    since,
		Status:      status,
		TriggerName: strings.TrimSpace(triggerName),
		Limit:       limit,
	})
	if err != nil {
		return err
	}
	return renderFiringsTable(cmd.OutOrStdout(), firings)
}

func renderFiringsTable(w io.Writer, firings []runtimetypes.EventFiring) error {
	if len(firings) == 0 {
		fmt.Fprintln(w, "(no firings)")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NID\tTRIGGER\tSTATUS\tREQUEST\tTIME\tERROR")
	for _, f := range firings {
		// UpdatedAt is the outcome time; CreatedAt is when the claim was taken.
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			f.NID, f.TriggerName, f.Status, f.RequestID,
			f.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"), compactLine(f.Error, 80))
	}
	return tw.Flush()
}

const doctorFiringWindow = 50

func printFiringTrouble(ctx context.Context, w io.Writer, exec libdbexec.Exec, workspaceID string) {
	store, err := runtimetypes.NewEventFiringStore(exec, workspaceID)
	if err != nil {
		return
	}
	firings, err := store.ListEventFirings(ctx, runtimetypes.EventFiringFilter{Limit: doctorFiringWindow})
	if err != nil {
		return
	}
	// A stranded firing counts too: its claim outlived its host.
	now := time.Now().UTC()
	bad := 0
	for _, f := range firings {
		if f.Status == runtimetypes.EventFiringStatusError || f.Status == runtimetypes.EventFiringStatusRefused || f.Stranded(now) {
			bad++
		}
	}
	if bad == 0 {
		return
	}
	fmt.Fprintf(w, "Event firings: %d of the last %d ended in error/refused or are stranded mid-run — contenox events firings\n", bad, len(firings))
}

func runEventsPrune(cmd *cobra.Command, args []string) error {
	keepDays, _ := cmd.Flags().GetInt("keep-days")
	yes, _ := cmd.Flags().GetBool("yes")
	if keepDays < 0 {
		return fmt.Errorf("--keep-days must be non-negative, got %d", keepDays)
	}

	db, _, err := openConfigDB(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.NewEventStore(db)
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays)
	cutoffPeriod := cutoff.Format("20060102")

	parts, err := store.ListEventPartitions(ctx)
	if err != nil {
		return err
	}
	candidates := 0
	for _, p := range parts {
		if p.Period < cutoffPeriod {
			candidates++
		}
	}
	out := cmd.OutOrStdout()
	if candidates == 0 {
		fmt.Fprintf(out, "Nothing to prune: no event partitions older than %d day(s).\n", keepDays)
		return nil
	}
	if !yes {
		fmt.Fprintf(out, "Drop %d event partition(s) older than %s (their events are gone for good)? [y/N]: ", candidates, cutoffPeriod)
		reader := bufio.NewReader(cmd.InOrStdin())
		answer, _ := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}
	dropped, err := store.PruneEventPartitionsBefore(ctx, cutoff)
	for _, period := range dropped {
		fmt.Fprintf(out, "dropped %s\n", period)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Pruned %d partition(s); events from the last %d day(s) are retained.\n", len(dropped), keepDays)
	return nil
}

func compactEventData(data json.RawMessage, max int) string {
	return compactLine(string(data), max)
}

func compactLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func formatTriggerLine(t eventtrigger.Trigger) string {
	line := fmt.Sprintf("%s → %s → %s", t.Name, t.ListenFor.Type, t.Chain)
	if t.Policy != "" {
		line += fmt.Sprintf(" (policy %s)", t.Policy)
	}
	return line
}

func printLoadedTriggers(ctx context.Context, w io.Writer, contenoxDir string) {
	res, err := eventtrigger.Load(ctx, nil, triggerRoots(contenoxDir)...)
	if err != nil {
		fmt.Fprintf(w, "Event triggers: discovery failed: %v\n", err)
		return
	}
	if len(res.Triggers) == 0 && len(res.Skipped) == 0 {
		return
	}
	fmt.Fprintf(w, "Event triggers (%d loaded):\n", len(res.Triggers))
	for _, t := range res.Triggers {
		fmt.Fprintf(w, "  %s\n", formatTriggerLine(t))
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(w, "  skipped: %s (%s)\n", s.Path, s.Reason)
	}
}

func triggerShadowNames(optInBeta bool, contenoxDir string) []string {
	if !optInBeta {
		return nil
	}
	seen := map[string]bool{}
	for _, dir := range triggerRoots(contenoxDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() && eventtrigger.IsTriggerFile(entry.Name()) {
				seen[entry.Name()] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveTriggerRef(contenoxDir string) func(name string) error {
	return func(name string) error {
		if name != filepath.Base(name) {
			return fmt.Errorf("must be a file name resolved from .contenox/ or ~/.contenox, not a path")
		}
		_, err := lookupSystemFile(contenoxDir, name)
		return err
	}
}
