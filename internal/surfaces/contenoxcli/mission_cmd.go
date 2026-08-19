package contenoxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/eventlog"
	"github.com/contenox/contenox/internal/services/fleetservice"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/missionservice"
	"github.com/contenox/contenox/internal/services/reportrouter"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
)

var missionCmd = &cobra.Command{
	Use:   "mission",
	Short: "Fire and inspect missions: unattended work orders with durable reports.",
	Long: `Fire missions at declared agents and read their durable records.

A mission is a one-line intent fired at a declared agent. The dispatched unit
runs unattended inside an envelope (a named HITL policy supplying its compute
ceilings, model/backend allowlists, and attention bounds; per-tool allow/deny
gating comes from the session profile's policy) and reports back through its
mission tools. Mission records and reports are
durable — list, show, and reports read them straight from the local database,
whether the mission was fired here, from an editor session (/mission), or is
still running.

` + toolGrantLine + `

` + askWaitLine + `

A mission PARKED on an ask has not stalled and is not lost: its unit
checkpointed and released its process, and the record stays open. The wait above
is the whole of its fate — answered, the unit resumes exactly once, wherever the
answer is given; expired, the on-timeout verdict (deny) applies and the unit
carries on with it; written 'never', it waits, across restarts, until somebody
answers. A parked mission is not reclaimed out from under an answerable ask:
the abandoned-mission sweep widens its silence bound to that ask's own window,
so it survives exactly as long as the ask can still be answered — and a mission
holding an ask with no deadline is never reclaimed at all. That last case is the
one way to park a mission open indefinitely, so 'contenox mission stop <id>' is
how you end one you no longer want.

'mission fire' embeds the fleet IN-PROCESS: the dispatched unit is a child
subprocess of THIS command, so --wait is required — when this process exits,
its units are torn down with it. Fire-and-detach needs a long-lived host: an
editor session ('contenox acp', the /mission command) or any other ACP client.

'mission asks' reads a mission's pending QUESTIONS — a unit's attention asks,
waiting on a human's own words rather than a yes/no. Answering them is NOT a
mission verb: 'contenox approvals respond <id> --answer "..."' is the one
surface that answers every pending ask, permission or question, mission-bound
or not — 'mission asks' only narrows the view to one mission (or every open
one).

Examples:
  contenox mission list
  contenox mission show <mission-id>
  contenox mission reports <mission-id>
  contenox mission plan <mission-id>
  contenox mission asks [mission-id]
  contenox mission fire agent-reviewer "review the open PR for regressions" --wait

Related:
  contenox approvals            the live ask queue — answer a pending question or permission gate
  contenox inbox                reports a mission left behind with no live session to read them

Related config (contenox config set …):
  default-mission-policy   the envelope used when --policy names none
  fleet-max-parallel       how many units may be open at once`,
}

var missionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recorded missions, newest first.",
	Long: `List missions from the durable store as a compact table of id, agent,
envelope, status, and age. Shows every recorded mission — open or finished,
fired from here or from an editor session.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, missions, err := openMissionService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		if reclaimed, sweepErr := missions.SweepAbandoned(ctx); sweepErr == nil {
			printReclaimedMissions(cmd.OutOrStdout(), reclaimed)
		}

		limit, _ := cmd.Flags().GetInt("limit")
		ms, err := missions.List(ctx, nil, limit)
		if err != nil {
			return fmt.Errorf("failed to list missions: %w", err)
		}
		return renderMissionTable(cmd.OutOrStdout(), ms, time.Now().UTC())
	},
}

var missionShowCmd = &cobra.Command{
	Use:   "show <mission-id>",
	Short: "Show one mission's record and its report summaries.",
	Long: `Print a mission's durable record — intent, agent, envelope, status, liveness —
followed by its reports (summaries and refs). A report the verification gate
downgraded (a claimed artifact that does not exist) shows its warning inline.
Use 'contenox mission reports <id>' for full report detail.

An open mission with no recent liveness is usually parked on an ask rather than
dead. A pending ask widens the mission's silence bound to that ask's own window,
so the abandon sweep will not reclaim it while the ask is still answerable; an
ask with no deadline holds it open with no bound at all. 'contenox approvals
list' shows what it is waiting on and how long that wait has left.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, missions, _, err := openMissionAndHitlServices(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		if reclaimed, sweepErr := missions.SweepAbandoned(ctx); sweepErr == nil {
			printReclaimedMissions(cmd.OutOrStdout(), reclaimed)
		}

		m, err := missions.Get(ctx, args[0])
		if err != nil {
			return fmt.Errorf("mission %q not found: %w — see 'contenox mission list' for recorded missions", args[0], err)
		}
		reports, err := missions.ListReports(ctx, m.ID, missionReportsReadLimit)
		if err != nil {
			return fmt.Errorf("failed to read reports for mission %q: %w", m.ID, err)
		}
		var asks []*runtimetypes.HITLApproval
		renderMissionShow(cmd.OutOrStdout(), m, reports, asks, time.Now().UTC())
		return nil
	},
}

var missionReportsCmd = &cobra.Command{
	Use:   "reports <mission-id>",
	Short: "Print a mission's reports in full detail.",
	Long: `Print every report a mission's unit filed — kind, summary, detail, refs, and
the structured hand-over when one is attached — oldest first, so the mission
reads chronologically.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := libtracker.WithNewRequestID(context.Background())
		db, missions, err := openMissionService(cmd)
		if err != nil {
			return err
		}
		defer db.Close()

		m, err := missions.Get(ctx, args[0])
		if err != nil {
			return fmt.Errorf("mission %q not found: %w — see 'contenox mission list' for recorded missions", args[0], err)
		}
		reports, err := missions.ListReports(ctx, m.ID, missionReportsReadLimit)
		if err != nil {
			return fmt.Errorf("failed to read reports for mission %q: %w", m.ID, err)
		}
		renderMissionReports(cmd.OutOrStdout(), m, reports)
		return nil
	},
}

var missionPlanCmd = &cobra.Command{
	Use:   "plan <mission-id>",
	Short: "Print a mission's living plan in full: every entry, its status and priority, and the revision history.",
	Long: `'mission show' prints only a one-line plan SUMMARY (revision number and
per-status counts) — this prints the plan itself: every entry, ordered, with
its status (pending|in_progress|completed) and priority, plus the durable
"+added/-removed — why" history of past revisions. A mission with no plan
(revision 0 — no resident planner has run for its agent) says so plainly.`,
	Args: cobra.ExactArgs(1),
	RunE: runMissionPlan,
}

var missionFireCmd = &cobra.Command{
	Use:   "fire <agent> <intent...>",
	Short: "Dispatch a mission in-process and wait for its terminal status.",
	Long: `Fire a one-line intent at a declared agent and wait for the mission to finish.

The fleet is embedded IN-PROCESS: the dispatched unit is a child subprocess of
this command, running unattended inside the envelope (--policy, or the
default-mission-policy config). Reports land in the durable mission store and
the operator inbox; read them with 'contenox mission show/reports'.

--wait is REQUIRED, and honestly so: the unit is a child of THIS process, so a
fired mission dies when this command exits — there is no detached fire from a
one-shot CLI. Fire-and-detach needs a long-lived host: an editor session
('contenox acp', the /mission command) or any other ACP client.

Exit status: 0 when the mission lands; non-zero when it derails, gets stuck, is
abandoned, or the wait times out (--timeout; on timeout the unit is torn down
with this process).

Examples:
  contenox mission fire agent-reviewer "review the open PR for regressions" --wait
  contenox mission fire agent-docs "update the README quickstart" --policy hitl-policy-strict.json --wait --timeout 15m

See declared agents with 'contenox agent list'.`,
	Args: cobra.MinimumNArgs(2),
	RunE: runMissionFire,
}

const missionWaitPollInterval = 2 * time.Second

const missionReportsReadLimit = 200

func runMissionFire(cmd *cobra.Command, args []string) error {
	wait, _ := cmd.Flags().GetBool("wait")
	if !wait {
		return fmt.Errorf("mission fire requires --wait: the dispatched unit is a child subprocess of this command and dies when it exits, so a detached fire from a one-shot CLI would tear its own mission down. Pass --wait to block until the mission finishes, or fire from a long-lived host (an editor session's /mission)")
	}

	out := cmd.OutOrStdout()
	policy, _ := cmd.Flags().GetString("policy")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	outcome, err := fireMissionAndWait(cmd, missionFireSpec{
		agent:   strings.TrimSpace(args[0]),
		intent:  strings.TrimSpace(strings.Join(args[1:], " ")),
		policy:  policy,
		timeout: timeout,
		narrate: out,
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(out, missionOutcomeLine(outcome.mission))
	if len(outcome.reports) > 0 {
		fmt.Fprintln(out, "Reports:")
		renderReportSummaries(out, outcome.reports)
	}
	fmt.Fprintf(out, "Full detail: contenox mission reports %s\n", outcome.mission.ID)
	if outcome.mission.Status != missionservice.StatusLanded {
		return &exitError{1}
	}
	return nil
}

// missionFireSpec is one in-process dispatch; narrate takes the progress lines `run` keeps off stdout.
type missionFireSpec struct {
	agent   string
	intent  string
	policy  string
	timeout time.Duration
	narrate io.Writer
}

type missionFireOutcome struct {
	mission *missionservice.Mission
	reports []*missionservice.Report
}

func fireMissionAndWait(cmd *cobra.Command, spec missionFireSpec) (*missionFireOutcome, error) {
	out := spec.narrate
	if out == nil {
		out = io.Discard
	}
	agentName := spec.agent
	intent := spec.intent

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, stopSignals := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx = libtracker.WithNewRequestID(ctx)

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	// A mission names the envelope that bounds it, so every declared one has to
	// be rendered before the name is looked up.
	if home, herr := globalContenoxDir(); herr == nil && home != contenoxDir {
		_, _ = syncEnvelopePolicies(home)
	}
	_, _ = syncEnvelopePolicies(contenoxDir)

	store := runtimetypes.New(db.WithoutTransaction())
	policy := strings.TrimSpace(spec.policy)
	if policy == "" {
		policy = strings.TrimSpace(clikv.Read(ctx, store, "default-mission-policy"))
	}
	if policy == "" {
		return nil, fmt.Errorf("no mission envelope: pass --policy <policy>, or set a default with `contenox config set default-mission-policy <policy>` — a mission must name the HITL policy that bounds it")
	}

	var tracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if trace, _ := cmd.Root().PersistentFlags().GetBool("trace"); trace {
		tracker = libtracker.NewTextActivityTracker(os.Stderr)
	}

	bus, err := substrate.OpenBus(ctx, db.WithoutTransaction())
	if err != nil {
		return nil, err
	}
	defer bus.Close()
	workspaceID := ResolveWorkspaceID(contenoxDir)
	trigHook := eventlog.NewTriggerHolder()
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionEventPublisher(ctx, db, bus, workspaceID, tracker, trigHook)))

	// One instance: a sibling cannot wake the waiters this one parked.
	var driverHITL hitlservice.Service
	optInBeta := betaEnabled(ctx, store)
	wantTriggers := false
	if optInBeta {
		if res, terr := loadTriggersKept(ctx, tracker, contenoxDir, true); terr == nil && len(res.Triggers) > 0 {
			wantTriggers = true
		}
	}
	var supervisor reportrouter.AgentSupervisor
	if wantTriggers {
		opts, optsErr := buildRunOpts(cmd, db, contenoxDir)
		if optsErr != nil {
			return nil, optsErr
		}
		opts.EffectiveDB = dbPath
		if wantTriggers && !cmd.Root().Flags().Changed("shell") {
			opts.EffectiveEnableLocalExec = true
		}
		engine, engErr := BuildEngine(ctx, db, opts)
		if engErr != nil {
			return nil, fmt.Errorf("mission fire: build engine for in-process dispatch: %w", engErr)
		}
		defer engine.Stop()
		trigHook.Set(buildInProcessTriggerHook(ctx, db, contenoxDir, workspaceID, engine, opts, cmd.ErrOrStderr()))
		// Deferred after engine.Stop so LIFO drains in-flight firings first.
		defer trigHook.Drain(eventlog.DefaultDrainTimeout)
	}

	if driverHITL == nil {
		driverHITL = newHITLService(ctx, contenoxDir, store, tracker, "")
	}

	projectRoot, _ := os.Getwd()
	fleet, _, stopFleet, err := fleetservice.BuildInProcess(ctx, fleetservice.InProcessDeps{
		DB:          db,
		Bus:         bus,
		Missions:    missions,
		ProjectRoot: projectRoot,
		WorkspaceID: workspaceID,
		// The unit must report into the database this fire resolved.
		DBPath:       dbPath,
		Tracker:      tracker,
		PolicySource: hitlPolicySource(contenoxDir),
		HITL:         driverHITL,
		DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
			discoverChainAgents(dctx, agents, contenoxDir, tracker, DiscoverDeps{Store: runtimetypes.New(db.WithoutTransaction()), Bus: bus})
		},
		AgentSupervisor: supervisor,
		Stderr:          os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	// Children die with the parent, on every exit path.
	defer stopFleet()

	timeout := spec.timeout
	res, err := fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:      agentName,
		Intent:         intent,
		HITLPolicyName: policy,
	})
	if err != nil {
		if hint := legacyChainPrefixHint(agentName); hint != "" && errors.Is(err, libdbexec.ErrNotFound) {
			return nil, fmt.Errorf("%w%s", err, hint)
		}
		return nil, err
	}
	fmt.Fprintf(out, "Mission fired at agent %q under envelope %q.\nIntent: %s\nMission %s (instance %s, session %s).\nWaiting for a terminal status (timeout %s; the unit is a child of this process and is torn down when it exits)…\n",
		agentName, policy, narratedIntent(intent), res.MissionID, res.InstanceID, res.SessionID, timeout)

	m, waitErr := waitForTerminalMission(ctx, missions, res.MissionID, missionWaitPollInterval, timeout)
	if waitErr != nil {
		switch {
		case errors.Is(waitErr, errMissionWaitTimeout):
			fmt.Fprintf(out, "mission %s did not finish within %s: it is torn down with this process (the unit is a child subprocess). Its record and any reports so far survive — see 'contenox mission show %s'. Re-run with a larger --timeout, or fire from a long-lived host (an editor session's /mission).\n",
				res.MissionID, timeout, res.MissionID)
		case errors.Is(waitErr, context.Canceled):
			fmt.Fprintf(out, "mission %s wait interrupted: the unit is torn down with this process. Its record and any reports so far survive — see 'contenox mission show %s'.\n",
				res.MissionID, res.MissionID)
		default:
			return nil, fmt.Errorf("waiting on mission %s: %w", res.MissionID, waitErr)
		}
		return nil, &exitError{1}
	}

	reports, rerr := missions.ListReports(ctx, m.ID, missionReportsReadLimit)
	if rerr != nil {
		reports = nil
	}
	return &missionFireOutcome{mission: m, reports: reports}, nil
}

func registerMissionFireFlags(beta bool) {
	missionFireCmd.ResetFlags()
	missionFireCmd.Flags().String("policy", "", "Mission envelope: the HITL policy bounding the unattended unit (default: the default-mission-policy config)")
	missionFireCmd.Flags().Bool("wait", false, "Block until the mission reaches a terminal status (REQUIRED: the unit is a child of this process and dies with it)")
	missionFireCmd.Flags().Duration("timeout", 30*time.Minute, "Maximum time to wait for a terminal status before tearing the unit down")
	if beta {
	}
}

var errMissionWaitTimeout = errors.New("mission wait timed out")

func waitForTerminalMission(ctx context.Context, missions missionservice.Service, id string, interval, timeout time.Duration) (*missionservice.Mission, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		m, err := missions.Get(ctx, id)
		if err == nil && m.Status != missionservice.StatusOpen {
			return m, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errMissionWaitTimeout
		case <-tick.C:
		}
	}
}

func missionOutcomeLine(m *missionservice.Mission) string {
	line := fmt.Sprintf("Mission %s finished: %s", m.ID, m.Status)
	if reason := strings.TrimSpace(m.StatusReason); reason != "" {
		line += " — " + reason
	}
	return line
}

func renderMissionTable(w io.Writer, missions []*missionservice.Mission, now time.Time) error {
	if len(missions) == 0 {
		fmt.Fprintln(w, "No missions recorded. Fire one with 'contenox mission fire <agent> \"<intent>\" --wait', or /mission from an editor session.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGENT\tENVELOPE\tSTATUS\tAGE")
	for _, m := range missions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.AgentName, m.HITLPolicyName, m.Status, formatMissionAge(now, m.CreatedAt))
	}
	return tw.Flush()
}

// narratedIntent keeps the fire narration one line: a piped stdin body rides
// inside the intent and would otherwise echo whole into the terminal or a CI
// log, so everything from the attachment marker on is summarized by size.
func narratedIntent(intent string) string {
	if i := strings.Index(intent, stdinBodyOpen); i >= 0 {
		return strings.TrimSpace(intent[:i]) + fmt.Sprintf(" (+ %d bytes piped stdin)", len(intent)-i)
	}
	return intent
}

func renderMissionShow(w io.Writer, m *missionservice.Mission, reports []*missionservice.Report, asks []*runtimetypes.HITLApproval, now time.Time) {
	fmt.Fprintf(w, "Mission:   %s\n", m.ID)
	fmt.Fprintf(w, "Intent:    %s\n", m.Intent)
	fmt.Fprintf(w, "Agent:     %s\n", m.AgentName)
	fmt.Fprintf(w, "Envelope:  %s\n", m.HITLPolicyName)
	status := string(m.Status)
	if reason := strings.TrimSpace(m.StatusReason); reason != "" {
		status += " — " + reason
	}
	fmt.Fprintf(w, "Status:    %s\n", status)
	if m.SessionID != "" {
		fmt.Fprintf(w, "Session:   %s\n", m.SessionID)
	}
	if m.InstanceID != "" {
		fmt.Fprintf(w, "Instance:  %s\n", m.InstanceID)
	}
	if m.ParentSessionID != "" {
		fmt.Fprintf(w, "Fired by:  session %s\n", m.ParentSessionID)
	}
	if m.LastHeartbeat != nil {
		fmt.Fprintf(w, "Heartbeat: %s ago\n", formatMissionAge(now, *m.LastHeartbeat))
	}
	if m.LastError != "" {
		fmt.Fprintf(w, "LastError: %s\n", m.LastError)
	}
	if m.Plan.Revision > 0 {
		pending, inProgress, completed := 0, 0, 0
		for _, e := range m.Plan.Entries {
			switch e.Status {
			case missionservice.PlanEntryPending:
				pending++
			case missionservice.PlanEntryInProgress:
				inProgress++
			case missionservice.PlanEntryCompleted:
				completed++
			}
		}
		fmt.Fprintf(w, "Plan:      revision %d (%d pending, %d in progress, %d completed)\n", m.Plan.Revision, pending, inProgress, completed)
	}
	fmt.Fprintf(w, "Created:   %s (%s ago)\n", m.CreatedAt.Format(time.RFC3339), formatMissionAge(now, m.CreatedAt))
	fmt.Fprintf(w, "Updated:   %s (%s ago)\n", m.UpdatedAt.Format(time.RFC3339), formatMissionAge(now, m.UpdatedAt))

	if len(asks) > 0 {
		fmt.Fprintf(w, "Asks:      %d pending — answer with 'contenox approvals respond <id> --answer \"...\"' (contenox mission asks %s):\n", len(asks), m.ID)
		for _, a := range asks {
			fmt.Fprintf(w, "             [%s] %s\n", a.ID, a.ArgsSummary)
		}
	}

	fmt.Fprintln(w)
	if len(reports) == 0 {
		fmt.Fprintln(w, "No reports filed yet.")
		return
	}
	fmt.Fprintf(w, "Reports (%d, oldest first — full detail: contenox mission reports %s):\n", len(reports), m.ID)
	renderReportSummaries(w, reports)
}

func renderReportSummaries(w io.Writer, reports []*missionservice.Report) {
	for _, r := range chronological(reports) {
		fmt.Fprintf(w, "  [%s] %s\n", r.Kind, r.Summary)
		if len(r.Refs) > 0 {
			fmt.Fprintf(w, "        refs: %s\n", strings.Join(r.Refs, ", "))
		}
		if warning := verificationWarningLine(r.Detail); warning != "" {
			fmt.Fprintf(w, "        ⚠ %s\n", warning)
		}
	}
}

func renderMissionReports(w io.Writer, m *missionservice.Mission, reports []*missionservice.Report) {
	fmt.Fprintf(w, "Mission %s (%s): %s\n", m.ID, m.Status, m.Intent)
	if len(reports) == 0 {
		fmt.Fprintln(w, "\nNo reports filed yet.")
		return
	}
	for _, r := range chronological(reports) {
		fmt.Fprintf(w, "\n[%s] %s\n", r.Kind, r.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "Summary: %s\n", r.Summary)
		if d := strings.TrimSpace(r.Detail); d != "" {
			fmt.Fprintf(w, "Detail:\n%s\n", d)
		}
		if len(r.Refs) > 0 {
			fmt.Fprintf(w, "Refs: %s\n", strings.Join(r.Refs, ", "))
		}
		if h := r.Handover; h != nil {
			fmt.Fprintln(w, "Handover:")
			if h.Outcome != "" {
				fmt.Fprintf(w, "  Outcome:  %s\n", h.Outcome)
			}
			if len(h.Artifacts) > 0 {
				fmt.Fprintf(w, "  Artifacts: %s\n", strings.Join(h.Artifacts, ", "))
			}
			if h.HandoverForNext != "" {
				fmt.Fprintf(w, "  For next: %s\n", h.HandoverForNext)
			}
			if h.Caveats != "" {
				fmt.Fprintf(w, "  Caveats:  %s\n", h.Caveats)
			}
		}
	}
}

func renderMissionPlan(w io.Writer, m *missionservice.Mission) {
	if m.Plan.Revision == 0 {
		fmt.Fprintf(w, "Mission %s has no plan yet (revision 0) — no resident planner has run for this mission.\n", m.ID)
		return
	}
	fmt.Fprintf(w, "Mission %s — plan revision %d\n", m.ID, m.Plan.Revision)
	if explanation := strings.TrimSpace(m.Plan.Explanation); explanation != "" {
		fmt.Fprintf(w, "Rationale: %s\n", explanation)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tPRIORITY\tENTRY")
	for _, e := range m.Plan.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Status, e.Priority, e.Content)
	}
	tw.Flush()

	if len(m.PlanRevisions) > 0 {
		fmt.Fprintln(w, "\nRevision history (oldest first):")
		for _, r := range m.PlanRevisions {
			line := fmt.Sprintf("  rev %d: +%d/-%d (%d pending, %d in progress, %d completed)",
				r.Revision, r.Added, r.Removed, r.Pending, r.InProgress, r.Completed)
			if explanation := strings.TrimSpace(r.Explanation); explanation != "" {
				line += " — " + explanation
			}
			fmt.Fprintln(w, line)
		}
	}
}

func chronological(reports []*missionservice.Report) []*missionservice.Report {
	out := make([]*missionservice.Report, len(reports))
	for i, r := range reports {
		out[len(reports)-1-i] = r
	}
	return out
}

const verificationWarningLead = "claimed artifacts not found"

func verificationWarningLine(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), verificationWarningLead) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func formatMissionAge(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

var missionStopCmd = &cobra.Command{
	Use:   "stop <mission-id>",
	Short: "Stop a running mission: abandon it, close its pending asks, and reap its unit wherever it runs",
	Long: `Finish a mission now as abandoned, close every ask it has pending, and reap its
unit in whichever process is hosting it.

This is the way out of a mission parked on an ask that will not be answered —
including one whose grant said timeout = "never", which no sweep will ever
expire and no abandon sweep will ever reclaim. Its closed asks resolve as
denials and the run checkpointed under each is dropped rather than resumed, so
the record says what happened rather than going quiet.`,
	Args: cobra.ExactArgs(1),
	RunE: runMissionStop,
}

func runMissionStop(cmd *cobra.Command, args []string) error {
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
	db, err := OpenDBAt(libtracker.WithNewRequestID(ctx), dbPath)
	if err != nil {
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	bus, err := substrate.OpenBus(ctx, db.WithoutTransaction())
	if err != nil {
		return err
	}
	defer bus.Close()
	missions := missionservice.New(db, missionservice.WithEventPublisher(missionEventPublisher(ctx, db, bus, ResolveWorkspaceID(contenoxDir), libtracker.NoopTracker{}, nil)))
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := newHITLService(ctx, contenoxDir, store, libtracker.NoopTracker{}, "")

	reason, _ := cmd.Flags().GetString("reason")
	if err := fleetservice.StopMission(ctx, missions, hitl, store, args[0], reason); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Mission %s abandoned. Pending asks are closed; any live unit is being reaped by its host process.\n", args[0])
	return nil
}

func runMissionPlan(cmd *cobra.Command, args []string) error {
	ctx := libtracker.WithNewRequestID(context.Background())
	db, missions, err := openMissionService(cmd)
	if err != nil {
		return err
	}
	defer db.Close()

	m, err := missions.Get(ctx, args[0])
	if err != nil {
		return fmt.Errorf("mission %q not found: %w — see 'contenox mission list' for recorded missions", args[0], err)
	}
	renderMissionPlan(cmd.OutOrStdout(), m)
	return nil
}

func openMissionService(cmd *cobra.Command) (io.Closer, missionservice.Service, error) {
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	missions, closer, err := publisherWiredMissions(dbCtx, cmd, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return closer, missions, nil
}

func publisherWiredMissions(ctx context.Context, cmd *cobra.Command, db libdbexec.DBManager) (missionservice.Service, io.Closer, error) {
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return missionservice.New(db), db, nil
	}
	bus, err := substrate.OpenBus(ctx, db.WithoutTransaction())
	if err != nil {
		return nil, nil, err
	}
	pub := missionEventPublisher(ctx, db, bus, ResolveWorkspaceID(contenoxDir), libtracker.NoopTracker{}, nil)
	missions := missionservice.New(db, missionservice.WithEventPublisher(pub))
	return missions, closerFunc(func() error {
		bus.Close()
		return db.Close()
	}), nil
}

func openMissionAndHitlServices(cmd *cobra.Command) (io.Closer, missionservice.Service, hitlservice.Service, error) {
	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid database path: %w", err)
	}
	dbCtx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(dbCtx, dbPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := newHITLService(dbCtx, contenoxDir, store, libtracker.NoopTracker{}, "")
	missions, closer, err := publisherWiredMissions(dbCtx, cmd, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, nil, err
	}
	return closer, missions, hitl, nil
}

func init() {
	missionListCmd.Flags().Int("limit", 50, "Maximum number of missions to list")
	registerMissionFireFlags(false)

	missionStopCmd.Flags().String("reason", "", "One line on why the mission is being stopped (persisted as the status reason)")

	missionCmd.AddCommand(missionListCmd)
	missionCmd.AddCommand(missionShowCmd)
	missionCmd.AddCommand(missionReportsCmd)
	missionCmd.AddCommand(missionPlanCmd)
	missionCmd.AddCommand(missionFireCmd)
	missionCmd.AddCommand(missionStopCmd)
}
