package contenoxcli

import (
	"context"
	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/localtools"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"io"
)

type chatOpts struct {
	// EffectiveTracker, when non-nil, overrides the engine's tracker.
	EffectiveTracker             libtracker.ActivityTracker
	EffectiveDB                  string
	EffectiveChain               string
	EffectiveDefaultModel        string
	EffectiveDefaultProvider     string
	EffectiveConfiguredModel     string
	EffectiveConfiguredProvider  string
	EffectiveAltDefaultModel     string
	EffectiveAltDefaultProvider  string
	EffectiveMaxTokens           string
	EffectiveContext             int
	EffectiveNoDeleteModels      bool
	EffectiveEnableLocalExec     bool
	EffectiveLocalExecAllowedDir string
	EffectiveTracing             bool
	EffectiveSteps               bool
	EffectiveHITL                bool
	EffectiveRaw                 bool
	EffectiveThink               string
	// EffectiveOptInBeta gates beta feature registration.
	EffectiveOptInBeta bool
	HistoryTrim        int
	LastN              int
	InputValue         string
	InputFlagPassed    bool
	// AttachPaths are --attach image files, riding the turn's user message.
	AttachPaths []string
	ContenoxDir string
	// EffectiveSkipBackendCycle skips state.RunBackendCycle.
	EffectiveSkipBackendCycle bool
	// EffectiveAskApproval lets editor integrations supply their own HITL UI.
	EffectiveAskApproval localtools.AskApproval
	// EffectiveHITLService overrides the hitlservice.Service BuildEngine would
	// mint; ignored when EffectiveHITL is false.
	EffectiveHITLService hitlservice.Service
	// EffectiveTaskEventSink receives task events without a bus subscription.
	EffectiveTaskEventSink taskengine.TaskEventSink
	// EffectiveExtraTools are host-scoped tool providers merged into this engine's
	// toolset and nowhere else.
	EffectiveExtraTools map[string]taskengine.ToolsRepo
	// WarnW is where engine construction prints operator warnings; nil silences.
	WarnW io.Writer
	// EffectiveStreamOutput renders assistant prose to stdout as it arrives.
	EffectiveStreamOutput bool
}

func buildTemplateVars(opts chatOpts) map[string]string {
	templateVars := map[string]string{
		"model":    opts.EffectiveDefaultModel,
		"provider": opts.EffectiveDefaultProvider,
		"think":    opts.EffectiveThink,
	}

	defaultModel := opts.EffectiveConfiguredModel
	if defaultModel == "" {
		defaultModel = opts.EffectiveDefaultModel
	}
	if defaultModel != "" {
		templateVars["default_model"] = defaultModel
	}

	defaultProvider := opts.EffectiveConfiguredProvider
	if defaultProvider == "" {
		defaultProvider = opts.EffectiveDefaultProvider
	}
	if defaultProvider != "" {
		templateVars["default_provider"] = defaultProvider
	}

	if opts.EffectiveAltDefaultModel != "" {
		templateVars["alt_model"] = opts.EffectiveAltDefaultModel
	}
	if opts.EffectiveAltDefaultProvider != "" {
		templateVars["alt_provider"] = opts.EffectiveAltDefaultProvider
	}
	if opts.EffectiveMaxTokens != "" {
		templateVars["max_tokens"] = opts.EffectiveMaxTokens
	}
	return templateVars
}
func buildRunOpts(cmd *cobra.Command, db libdbexec.DBManager, contenoxDir string) (chatOpts, error) {
	flags := cmd.Root().Flags()

	ctx := libtracker.WithNewRequestID(context.Background())
	store := runtimetypes.New(db.WithoutTransaction())

	// Read persistent defaults from SQLite KV; flags always override.
	kvModel, _ := getConfigKV(ctx, store, "default-model")
	kvProvider, _ := getConfigKV(ctx, store, "default-provider")
	kvAltModel, _ := getConfigKV(ctx, store, "default-alt-model")
	kvAltProvider, _ := getConfigKV(ctx, store, "default-alt-provider")
	effectiveMaxTokens, err := resolveEffectiveMaxTokens(ctx, store, flags)
	if err != nil {
		return chatOpts{}, err
	}
	effectiveThink, err := resolveEffectiveThink(ctx, store, flags)
	if err != nil {
		return chatOpts{}, err
	}

	effectiveModel, _ := flags.GetString("model")
	if !flags.Changed("model") && (effectiveModel == "" || effectiveModel == defaultModel) {
		if kvModel != "" {
			effectiveModel = kvModel
		} else {
			effectiveModel = defaultModel
		}
	}

	effectiveDefaultProvider := kvProvider
	if flags.Changed("provider") {
		if v, _ := flags.GetString("provider"); v != "" {
			effectiveDefaultProvider = v
		}
	}

	effectiveAltModel := kvAltModel
	if flags.Changed("alt-model") {
		if v, _ := flags.GetString("alt-model"); v != "" {
			effectiveAltModel = v
		}
	}

	effectiveAltProvider := kvAltProvider
	if flags.Changed("alt-provider") {
		if v, _ := flags.GetString("alt-provider"); v != "" {
			effectiveAltProvider = v
		}
	}

	effectiveContext, _ := flags.GetInt("context")
	effectiveTracing, _ := flags.GetBool("trace")

	effectiveEnableLocalExec, _ := flags.GetBool("shell")
	effectiveLocalExecAllowedDir, _ := flags.GetString("local-exec-allowed-dir")
	autoMode, _ := cmd.Flags().GetBool("auto")
	effectiveHITL := !autoMode

	return chatOpts{
		EffectiveDB:                  "", // resolved separately in RunE
		EffectiveChain:               "", // unused — run loads chain directly
		EffectiveContext:             effectiveContext,
		EffectiveDefaultModel:        effectiveModel,
		EffectiveDefaultProvider:     effectiveDefaultProvider,
		EffectiveConfiguredModel:     kvModel,
		EffectiveConfiguredProvider:  kvProvider,
		EffectiveAltDefaultModel:     effectiveAltModel,
		EffectiveAltDefaultProvider:  effectiveAltProvider,
		EffectiveMaxTokens:           effectiveMaxTokens,
		EffectiveNoDeleteModels:      true,
		EffectiveEnableLocalExec:     effectiveEnableLocalExec,
		EffectiveLocalExecAllowedDir: effectiveLocalExecAllowedDir,
		EffectiveHITL:                effectiveHITL,
		EffectiveTracing:             effectiveTracing,
		EffectiveThink:               effectiveThink,
		EffectiveOptInBeta:           betaEnabled(ctx, store),
		ContenoxDir:                  contenoxDir,

		WarnW: cmd.ErrOrStderr(),
	}, nil
}
