package contenoxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentservice"
	"github.com/contenox/beam/internal/services/gointel"
	"github.com/contenox/beam/internal/services/gojatool"
	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/internal/services/jqtool"
	"github.com/contenox/beam/internal/services/localtools"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/contenox/beam/internal/services/searchtool"
	"github.com/contenox/beam/internal/services/setupcheck"
	"github.com/contenox/beam/internal/store/runtimetypes"
)

// ComputeReadiness builds the engine — which runs a read-only backend sync, NOT
// a model completion — and returns the evaluated setup readiness. It is the
// shared path behind `contenox doctor` and the setup wizard's final check, so
// readiness is verified without ever sending a prompt (which would run a chain,
// spend tokens, and could touch tools/audit trails). opts.EffectiveNoDeleteModels
// is honored by BuildEngine, so a readiness check never prunes models.
func ComputeReadiness(ctx context.Context, db libdbexec.DBManager, opts chatOpts) (setupcheck.Result, error) {
	engine, err := BuildEngine(ctx, db, opts)
	if err != nil {
		return setupcheck.Result{}, err
	}
	defer engine.Stop()
	return setupcheck.EnrichResultWithOllamaProbe(ctx, engine.SetupCheck), nil
}

type Engine = enginesvc.Engine

func BuildEngine(ctx context.Context, db libdbexec.DBManager, opts chatOpts) (*Engine, error) {
	var tracker libtracker.ActivityTracker = libtracker.NoopTracker{}
	if opts.EffectiveTracing {
		// nil logger = the process default handler, which is whatever this
		// surface's composition root pointed it at (stderr for the CLI,
		// beam.log for beam — see redirectBeamLogsToFile). Passing nil rather
		// than slog.Default() is the same thing one indirection later, and it
		// keeps this file out of the log/slog import graph: slog is the
		// tracker's output sink, not an API command logic calls.
		tracker = libtracker.NewLogActivityTracker(nil)
	}

	reportErr, reportChange, end := tracker.Start(ctx, "build", "engine")
	defer end()

	// The Go-intelligence index owns a reaper goroutine, so it needs a
	// lifecycle: it rides engine.Stop below, and the guard here covers every
	// error return between construction and that wrap.
	// CwdResolver is what makes the index usable when no allowed dir was
	// declared: without it gointel advertises six tools and refuses every
	// call for want of a workspace root, while local_fs works beside it on
	// the same directory. AllowedDir still WINS when set — this only
	// supplies the root that was otherwise missing, and containment is
	// unchanged either way.
	goIndex := gointel.NewIndex(gointel.Config{
		AllowedDir: opts.EffectiveLocalExecAllowedDir,
		CwdResolver: func(context.Context) string {
			cwd, err := os.Getwd()
			if err != nil {
				return ""
			}
			return cwd
		},
	})
	// The goja sandbox: goja_eval plus one tool per operator-authored script in
	// $CONTENOX_DIR/tools. Construction LOADS, executes and validates every script
	// file, so a broken script is a startup error that names the file — the
	// blueprint's fail-fast rule, and the whole reliability story for the feature:
	// a silently skipped script is a tool the operator believes exists, the model
	// never sees, and nothing ever complains about.
	gt, err := gojatool.New(gojatool.Config{ScriptDir: filepath.Join(opts.ContenoxDir, "tools")})
	if err != nil {
		goIndex.Shutdown()
		reportErr(err)
		return nil, err
	}

	engineBuilt := false
	defer func() {
		if !engineBuilt {
			goIndex.Shutdown()
			gt.Shutdown()
		}
	}()

	tools := localToolset(opts, db, tracker, goIndex, gt)

	askApproval := opts.EffectiveAskApproval
	if askApproval == nil {
		askApproval = NewCLIAskApproval(os.Stderr)
	}

	readinessModel, readinessProvider := readinessDefaults(opts)

	// Build the HITL service HERE and inject it, rather than letting enginesvc
	// mint one internally: the composition root must hold the instance so it can
	// register the resume-on-verdict hook after the engine exists (the engine is
	// a hook dependency, so registration cannot happen anywhere lower).
	var hitlSvc hitlservice.Service
	if opts.EffectiveHITL {
		hitlSvc = hitlservice.NewWithDefaultPolicy(hitlPolicySource(opts.ContenoxDir), runtimetypes.LocalTenantID, runtimetypes.New(db.WithoutTransaction()), tracker, "")
	}

	reportChange("phase", "tools_prepared")
	engine, err := enginesvc.Build(ctx, db, enginesvc.Config{
		DefaultModel:             opts.EffectiveDefaultModel,
		DefaultProvider:          opts.EffectiveDefaultProvider,
		AltDefaultModel:          opts.EffectiveAltDefaultModel,
		AltDefaultProvider:       opts.EffectiveAltDefaultProvider,
		ReadinessDefaultModel:    readinessModel,
		ReadinessDefaultProvider: readinessProvider,
		ContextLength:            opts.EffectiveContext,
		NoDeleteModels:           opts.EffectiveNoDeleteModels,
		LocalTools:               tools,
		EnableHITL:               opts.EffectiveHITL,
		AskApproval:              askApproval,
		HITLService:              hitlSvc,
		Tracker:                  tracker,
		Tracing:                  opts.EffectiveTracing,
		SkipBackendCycle:         opts.EffectiveSkipBackendCycle,
		WorkspaceID:              ResolveWorkspaceID(opts.ContenoxDir),
		HITLPolicySource:         hitlPolicySource(opts.ContenoxDir),
		TaskEventSink:            opts.EffectiveTaskEventSink,
		// Close the goja sandbox's construction cycle: host.tool needs the
		// aggregate repo that the sandbox is itself registered inside, and that
		// repo cannot exist until this Build assembles it. enginesvc hands back
		// the HITL-WRAPPED repo here (see OnToolsRepoReady), which is the one
		// boundary rule made real — a script calling local_fs.write_file meets
		// exactly the envelope a model call would, so script tools inherit the
		// entire policy story instead of quietly bypassing it.
		OnToolsRepoReady: func(repo taskengine.ToolsRepo) {
			gt.SetHost(gojatool.HostFromRepo(repo))
		},
	})
	if err != nil {
		reportErr(err)
		return nil, err
	}
	// The engine now has the ONE resolved model route, so workspace_search can
	// finally be given the embedding seam it was registered without.
	bindWorkspaceSearch(tools, db, engine)
	if hitlSvc != nil {
		hitlservice.SetResumeHook(hitlSvc, agentservice.ResumeHook(agentservice.Deps{
			Engine:      engine,
			DB:          db,
			WorkspaceID: ResolveWorkspaceID(opts.ContenoxDir),
		}))
	}
	reportChange("phase", "enginesvc_built")
	// Ride the engine's chainable stop so the index's reaper is joined
	// whenever the engine goes down — the same pattern enginesvc uses for
	// the MCP manager.
	engineBuilt = true
	oldStop := engine.Stop
	engine.Stop = func() {
		goIndex.Shutdown()
		// The sandbox refuses further executions and joins in-flight ones (bounded
		// — an in-flight host.tool call may be parked on a human approval).
		gt.Shutdown()
		oldStop()
	}
	return engine, nil
}

// localToolset is the set of local tool providers every CLI-side engine gets.
// It is a function rather than an inline literal so what is registered — and,
// just as importantly, what is registered only when asked for — is assertable
// without standing up a whole engine (engine_test.go).
func localToolset(opts chatOpts, db libdbexec.DBManager, tracker libtracker.ActivityTracker, goIndex gointel.Index, gojaTools *gojatool.Toolset) map[string]taskengine.ToolsRepo {
	tools := map[string]taskengine.ToolsRepo{
		"echo":     localtools.NewEchoTools(),
		"print":    localtools.NewPrint(tracker),
		"webtools": localtools.NewWebCaller(tracker),
		"local_fs": localtools.NewLocalFSTools(opts.EffectiveLocalExecAllowedDir, db),
		// git is ALWAYS registered — unlike local_shell, it is not gated on
		// --shell. Reading the repository is the point: an agent that cannot see
		// what changed says "I can't do that" to the most ordinary question a
		// coding session has. What it may DO is the envelope's call, not this
		// file's: the seeded policies allow the six read operations and hold the
		// four that change the repository at an approval. Same directory scoping
		// as local_fs above, so both tools see one workspace.
		localtools.GitToolsName: localtools.NewGitTools(opts.EffectiveLocalExecAllowedDir),
		// Go intelligence is ALWAYS registered for the same reason git is:
		// asking "what type is this / who calls this" is reading, and every
		// gointel tool is a read (the seeded policies allow the whole
		// toolset — revisit that rule before any mutating op ever lands
		// here). Same directory scoping as local_fs and git.
		gointel.ToolsProviderName: gointel.NewTools(goIndex),
		// jq is ALWAYS registered, for the same reason gointel and git are, and
		// for one more that is specific to this runtime: contenox's OWN
		// configuration surface is JSON. Chain files, hitl-policy files and agent
		// definitions are the documents an agent is most often asked about, and
		// answering ".tasks[] | select(.handler==\"tools\")" by reading a 900-line
		// file into the context window costs the whole file, every turn it stays
		// there. Same directory scoping as local_fs, git and gointel, so all four
		// see one workspace; the seeded policies allow the toolset because its one
		// operation reads a file read_file already reaches, writes nothing,
		// reaches no network, and is deadline-bounded including recursion (see
		// internal/services/jqtool for the argument in full).
		jqtool.ToolsProviderName: jqtool.NewTools(opts.EffectiveLocalExecAllowedDir),
		// Workspace semantic search is ALWAYS registered for the same reason
		// gointel and git are: it is a READ of files the agent may already
		// read, returning file:line-range citations from the index `contenox
		// index` built (the seeded policies allow the toolset — it has exactly
		// one operation and that operation writes nothing). It is registered
		// UNBOUND here and completed by bindWorkspaceSearch below, because the
		// embedding seam it needs is an output of the engine this map is an
		// input to; see workspaceSearchRepo in index_cmd.go.
		searchtool.ToolsProviderName: newWorkspaceSearchTools(ResolveWorkspaceID(opts.ContenoxDir)),
		// goja is ALWAYS registered, and registering it costs nothing an operator
		// did not ask for: with no $CONTENOX_DIR/tools directory the provider
		// carries exactly one tool, goja_eval, a pure compute sandbox with no
		// ambient I/O at all (no filesystem, no network, no require) — the model's
		// only reach out of it is host.tool, which re-enters this very map through
		// the envelope. What it may DO is therefore the envelope's call, as ever:
		// the seeded policies ALLOW goja_eval — there is nothing for an approval
		// to protect in a sandbox with no ambient I/O, and every host.tool call it
		// makes is gated on its own merits — and give script tools no rule at all,
		// so operator-authored code falls to default_action.
		gojatool.ToolsProviderName: gojaTools,
		// Durable-only mission tools (no asker, no publisher): this engine is
		// what `approvals respond` resumes suspended MISSION chains on, and a
		// resumed chain's later mission_report/mission_finish calls must land
		// in the store rather than fail as unknown tools. A nested attention
		// ask degrades to the durable blocker fallback here.
		missiontools.ToolsProviderName: missiontools.New(missionservice.New(db), nil),
	}
	if opts.EffectiveEnableLocalExec {
		execOpts := []localtools.LocalExecOption{}
		if opts.EffectiveLocalExecAllowedDir != "" {
			execOpts = append(execOpts, localtools.WithLocalExecAllowedDir(opts.EffectiveLocalExecAllowedDir))
		}
		tools["local_shell"] = localtools.NewLocalExecTools(execOpts...)

		// A message for the operator, NOT telemetry. Reaching this line takes an
		// explicit --auto (effectiveHITL is !autoMode), so the human approval
		// gate was removed on purpose and the only thing still standing between
		// the model and an arbitrary command is whatever the chain's
		// tools_policies happens to say. The person who can fix that is at the
		// terminal right now and the fix is one flag, so it belongs on stderr in
		// the command's voice — a tracker report would be silent unless --trace
		// was also passed, i.e. absent in exactly the default case that matters.
		if !opts.EffectiveHITL && opts.EffectiveLocalExecAllowedDir == "" && opts.WarnW != nil {
			fmt.Fprint(opts.WarnW, "warning: --auto disabled the approval prompt and local_shell has no allowed dir — the agent may run any command, anywhere.\n"+
				"         scope it with: --local-exec-allowed-dir .\n")
		}
	}
	return tools
}

// readinessDefaults derives the effective default model/provider to credit during
// setup preflight. It credits the effective value whenever a flag override made it
// DIFFER from persisted config — not only when config is empty. The setup-readiness
// check must validate the model/provider the engine will ACTUALLY use for the turn
// (opts.EffectiveDefault*, which honor --model/--provider), so an explicit override
// to a healthy backend is not blocked by a broken persisted default: `--provider
// vertex-google` must run even when default-provider=ollama is configured but unservable.
// A model equal to the hardcoded fallback with no persisted config is still treated
// as unset (matching the flag-vs-config precedence in cli.go/run_cmd.go); when the
// effective value equals persisted config, the check sees that value directly and no
// override is needed.
func readinessDefaults(opts chatOpts) (model, provider string) {
	if opts.EffectiveDefaultModel != opts.EffectiveConfiguredModel &&
		opts.EffectiveDefaultModel != "" &&
		opts.EffectiveDefaultModel != defaultModel {
		model = opts.EffectiveDefaultModel
	}
	if opts.EffectiveDefaultProvider != opts.EffectiveConfiguredProvider &&
		opts.EffectiveDefaultProvider != "" {
		provider = opts.EffectiveDefaultProvider
	}
	return model, provider
}

// hitlPolicySource builds the CLI's HITL policy lookup: the workspace .contenox
// dir first, then the user's ~/.contenox as fallback.
func hitlPolicySource(primaryDir string) hitlservice.PolicySource {
	dirs := []string{primaryDir}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".contenox"))
	}
	return hitlservice.NewFSPolicySource(dirs...)
}
