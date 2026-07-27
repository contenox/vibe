// mission_cmd.go holds the `contenox mission` verbs: durable reads over the
// mission store (list/show/reports) and the blocking in-process fire
// (`mission fire --wait`). The verbs are THIN — reads go straight through
// missionservice over the same SQLite the editor writes, and fire composes the
// fleet through fleetservice.BuildInProcess (the same service constructor the
// ACP editor embeds) — per the build-on-services rule: no orchestration lives
// here, only flags, rendering, and exit codes.
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

	"github.com/contenox/beam/internal/libbus"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/services/fleetservice"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/spf13/cobra"
)

var missionCmd = &cobra.Command{
	Use:   "mission",
	Short: "Fire and inspect missions: unattended work orders with durable reports.",
	Long: `Fire missions at declared agents and read their durable records.

A mission is a one-line intent fired at a declared agent. The dispatched unit
runs unattended inside an envelope (a named HITL policy that bounds what it may
do) and reports back through its mission tools. Mission records and reports are
durable — list, show, and reports read them straight from the local database,
whether the mission was fired here, from an editor session (/mission), or is
still running.

'mission fire' embeds the fleet IN-PROCESS: the dispatched unit is a child
subprocess of THIS command, so --wait is required — when this process exits,
its units are torn down with it. Fire-and-detach needs a long-lived host: an
editor session ('contenox acp', the /mission command) today, beam later.

Examples:
  contenox mission list
  contenox mission show <mission-id>
  contenox mission reports <mission-id>
  contenox mission fire agent-reviewer "review the open PR for regressions" --wait

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
Use 'contenox mission reports <id>' for full report detail.`,
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
		renderMissionShow(cmd.OutOrStdout(), m, reports, time.Now().UTC())
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
('contenox acp', the /mission command) today, beam later.

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

// missionWaitPollInterval is how often `mission fire --wait` re-reads the
// mission record while waiting for a terminal status. Polling (not the bus) is
// deliberate for a one-shot verb: the terminal status is a durable fact, and a
// couple of seconds of read latency is invisible next to a mission's runtime.
const missionWaitPollInterval = 2 * time.Second

// missionReportsReadLimit bounds how many reports the read verbs fetch — ample
// for a single mission (units file a handful; the runtime files one blocker).
const missionReportsReadLimit = 200

func runMissionFire(cmd *cobra.Command, args []string) error {
	wait, _ := cmd.Flags().GetBool("wait")
	if !wait {
		return fmt.Errorf("mission fire requires --wait: the dispatched unit is a child subprocess of this command and dies when it exits, so a detached fire from a one-shot CLI would tear its own mission down. Pass --wait to block until the mission finishes, or fire from a long-lived host (an editor session's /mission)")
	}

	out := cmd.OutOrStdout()
	agentName := strings.TrimSpace(args[0])
	intent := strings.TrimSpace(strings.Join(args[1:], " "))

	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, stopSignals := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctx = libtracker.WithNewRequestID(ctx)

	contenoxDir, err := ResolveContenoxDir(cmd)
	if err != nil {
		return fmt.Errorf("failed to resolve .contenox dir: %w", err)
	}
	dbPath, err := resolveDBPath(cmd)
	if err != nil {
		return err
	}
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open database %q: %w", dbPath, err)
	}
	defer db.Close()

	// Seed the embedded HITL policy presets into ~/.contenox (best-effort, never
	// overwriting) so the default envelopes exist on a box that has not run
	// `contenox acp` yet. A failure is not fatal: the policy validator below
	// refuses a dispatch whose envelope genuinely cannot be loaded, with a
	// message naming the fix.
	if home, herr := globalContenoxDir(); herr == nil {
		_ = writeEmbeddedHITLPolicies(home, false)
	}

	store := runtimetypes.New(db.WithoutTransaction())
	policy, _ := cmd.Flags().GetString("policy")
	policy = strings.TrimSpace(policy)
	if policy == "" {
		policy = strings.TrimSpace(clikv.Read(ctx, store, "default-mission-policy"))
	}
	if policy == "" {
		return fmt.Errorf("no mission envelope: pass --policy <policy>, or set a default with `contenox config set default-mission-policy <policy>` — a mission must name the HITL policy that bounds it")
	}

	var tracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if trace, _ := cmd.Root().PersistentFlags().GetBool("trace"); trace {
		tracker = libtracker.NewTextActivityTracker(os.Stderr)
	}

	// The ONE bus this process owns, shared between the mission store's publisher
	// and the fleet's report router (built inside BuildInProcess), so a unit's
	// cross-process ReportAddedEvent reaches the router and falls through to the
	// operator inbox — this operator-fired mission has no parent session.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))

	// A dispatched mission's cwd defaults to this command's working directory
	// when the request names none — the same default the editor uses.
	projectRoot, _ := os.Getwd()
	fleet, _, stopFleet, err := fleetservice.BuildInProcess(ctx, fleetservice.InProcessDeps{
		DB:           db,
		Bus:          bus,
		Missions:     missions,
		ProjectRoot:  projectRoot,
		Tracker:      tracker,
		PolicySource: hitlPolicySource(contenoxDir),
		DiscoverAgents: func(dctx context.Context, agents agentregistryservice.Service) {
			discoverChainAgents(dctx, agents, contenoxDir)
		},
		// No SessionDeliverer or AgentSupervisor: this process hosts no chat
		// sessions, so kernel-only delivery is right and every report of this
		// parentless mission lands in the operator inbox / durable store.
		Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	// Children die with the parent: the teardown Closes the kernel, reaping the
	// dispatched unit, on every exit path — landed, failed, timed out, or Ctrl-C.
	defer stopFleet()

	timeout, _ := cmd.Flags().GetDuration("timeout")
	res, err := fleet.Dispatch(ctx, fleetservice.DispatchRequest{
		AgentName:      agentName,
		Intent:         intent,
		HITLPolicyName: policy,
		// ParentSessionID empty: an operator fired this directly, so reports route
		// to the durable store and operator inbox rather than a chat session.
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Mission fired at agent %q under envelope %q.\nIntent: %s\nMission %s (instance %s, session %s).\nWaiting for a terminal status (timeout %s; the unit is a child of this process and is torn down when it exits)…\n",
		agentName, policy, intent, res.MissionID, res.InstanceID, res.SessionID, timeout)

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
			return fmt.Errorf("waiting on mission %s: %w", res.MissionID, waitErr)
		}
		return &exitError{1}
	}

	fmt.Fprintln(out, missionOutcomeLine(m))
	if reports, rerr := missions.ListReports(ctx, m.ID, missionReportsReadLimit); rerr == nil && len(reports) > 0 {
		fmt.Fprintln(out, "Reports:")
		renderReportSummaries(out, reports)
	}
	fmt.Fprintf(out, "Full detail: contenox mission reports %s\n", m.ID)
	if m.Status != missionservice.StatusLanded {
		return &exitError{1}
	}
	return nil
}

// errMissionWaitTimeout reports that a mission was still open when the wait
// budget ran out — a branchable sentinel so the caller can render the honest
// teardown message and choose the exit code.
var errMissionWaitTimeout = errors.New("mission wait timed out")

// waitForTerminalMission polls the mission store every interval until mission
// id reaches a terminal status (anything but open), the timeout elapses
// (errMissionWaitTimeout), or ctx is cancelled. A transient read error does not
// abort the wait — the next tick retries — but an immediately-unreadable
// mission would surface on the first read the caller performed before waiting.
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

// missionOutcomeLine renders the one-line terminal verdict `mission fire
// --wait` prints: the status, and the reason when the mission recorded one.
func missionOutcomeLine(m *missionservice.Mission) string {
	line := fmt.Sprintf("Mission %s finished: %s", m.ID, m.Status)
	if reason := strings.TrimSpace(m.StatusReason); reason != "" {
		line += " — " + reason
	}
	return line
}

// ─── rendering ──────────────────────────────────────────────────────────────

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

func renderMissionShow(w io.Writer, m *missionservice.Mission, reports []*missionservice.Report, now time.Time) {
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

	fmt.Fprintln(w)
	if len(reports) == 0 {
		fmt.Fprintln(w, "No reports filed yet.")
		return
	}
	fmt.Fprintf(w, "Reports (%d, oldest first — full detail: contenox mission reports %s):\n", len(reports), m.ID)
	renderReportSummaries(w, reports)
}

// renderReportSummaries prints one line per report (oldest first), its refs,
// and — the honesty surface — the verification-gate warning when the gate
// downgraded the report because a claimed artifact did not exist.
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

// chronological returns reports oldest-first for reading. ListReports hands
// them back newest-first (the store's scan order); a mission reads honestly in
// the order its unit filed.
func chronological(reports []*missionservice.Report) []*missionservice.Report {
	out := make([]*missionservice.Report, len(reports))
	for i, r := range reports {
		out[len(reports)-1-i] = r
	}
	return out
}

// verificationWarningLead mirrors the stable, greppable lead the conclusion
// verification gate (missiontools' verify.go, verificationWarningLead) appends
// to a downgraded report's detail. Matched by string here because the gate's
// constant is unexported and the lead is contracted stable — "greppable" is its
// documented purpose, and missiontools' verify tests pin the literal.
const verificationWarningLead = "claimed artifacts not found"

// verificationWarningLine extracts the verification-gate warning from a
// report's detail, or "" when the gate never touched it — so `mission show`
// surfaces a downgraded result's warning without printing full detail.
func verificationWarningLine(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), verificationWarningLead) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// formatMissionAge renders a compact single-unit age ("45s", "12m", "3h",
// "2d") for the mission table and liveness lines.
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
	Args:  cobra.ExactArgs(1),
	RunE:  runMissionStop,
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

	// Publisher-wired on purpose: the terminal-status event is what reaches the
	// LIVE host of the unit (an editor process, a --wait fire in another
	// terminal) over the shared SQLite bus and makes it reap the subprocess.
	bus := libbus.NewSQLite(db.WithoutTransaction())
	defer bus.Close()
	missions := missionservice.New(db, missionservice.WithEventPublisher(bus))
	store := runtimetypes.New(db.WithoutTransaction())
	hitl := hitlservice.NewWithDefaultPolicy(hitlPolicySource(contenoxDir), runtimetypes.LocalTenantID, store, libtracker.NoopTracker{}, "")

	reason, _ := cmd.Flags().GetString("reason")
	if err := fleetservice.StopMission(ctx, missions, hitl, store, args[0], reason); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Mission %s abandoned. Pending asks are closed; any live unit is being reaped by its host process.\n", args[0])
	return nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

// openMissionService opens the shared database the way every other read verb
// does (OpenDBAt over resolveDBPath) and returns a missionservice over it. The
// read verbs need no bus: they consume durable facts, not events.
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
	return db, missionservice.New(db), nil
}

func init() {
	missionListCmd.Flags().Int("limit", 50, "Maximum number of missions to list")
	missionFireCmd.Flags().String("policy", "", "Mission envelope: the HITL policy bounding the unattended unit (default: the default-mission-policy config)")
	missionFireCmd.Flags().Bool("wait", false, "Block until the mission reaches a terminal status (REQUIRED: the unit is a child of this process and dies with it)")
	missionFireCmd.Flags().Duration("timeout", 30*time.Minute, "Maximum time to wait for a terminal status before tearing the unit down")

	missionStopCmd.Flags().String("reason", "", "One line on why the mission is being stopped (persisted as the status reason)")

	missionCmd.AddCommand(missionListCmd)
	missionCmd.AddCommand(missionShowCmd)
	missionCmd.AddCommand(missionReportsCmd)
	missionCmd.AddCommand(missionFireCmd)
	missionCmd.AddCommand(missionStopCmd)
}
