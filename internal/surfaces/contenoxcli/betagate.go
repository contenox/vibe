// betagate.go resolves the opt-in-beta gate once per invocation. Off means
// invisible: gated features are absent from registration seams (toolsets,
// help, discovery), never present-but-refused.
package contenoxcli

import (
	"context"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/gojatool"
	"github.com/contenox/contenox/internal/services/shellsession"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libtracker"
)

// optInBetaKey is the config key behind `contenox config set opt-in-beta`.
const optInBetaKey = "opt-in-beta"

// envOptInBeta overrides the stored opt-in-beta value for one invocation.
// "1"/"true" (case-insensitive) is on; any other non-empty value is off;
// empty or unset falls back to config.
const envOptInBeta = "CONTENOX_OPT_IN_BETA"

// betaEnvOverride reads the env override. ok is false when the variable is
// unset or empty, so config decides.
func betaEnvOverride() (on, ok bool) {
	v := strings.TrimSpace(os.Getenv(envOptInBeta))
	if v == "" {
		return false, false
	}
	switch strings.ToLower(v) {
	case "1", "true":
		return true, true
	default:
		return false, true
	}
}

// betaEnabled reports whether beta features are visible this invocation:
// the env override wins; otherwise the stored opt-in-beta config ("true" is
// on, anything else off).
func betaEnabled(ctx context.Context, store runtimetypes.Store) bool {
	if on, ok := betaEnvOverride(); ok {
		return on
	}
	return clikv.Read(ctx, store, optInBetaKey) == "true"
}

// betaEnabledGlobal resolves the gate with no command context (Main's help
// gating, `init --refresh-policies`): the env override, then opt-in-beta in
// the default global DB. An absent or unreadable DB resolves off.
func betaEnabledGlobal() bool {
	if on, ok := betaEnvOverride(); ok {
		return on
	}
	dbPath, err := globalDBPath()
	if err != nil {
		return false
	}
	if _, err := os.Stat(dbPath); err != nil {
		// Never create the DB just to answer a visibility question.
		return false
	}
	ctx := libtracker.WithNewRequestID(context.Background())
	db, err := OpenDBAt(ctx, dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	return clikv.Read(ctx, runtimetypes.New(db.WithoutTransaction()), optInBetaKey) == "true"
}

// betaGatedToolsets is the staleness detector's skip set: the toolset names
// invisible without the opt-in, nil when enabled. A policy file missing rules
// for an invisible toolset is not stale.
func betaGatedToolsets(enabled bool) map[string]bool {
	if enabled {
		return nil
	}
	return map[string]bool{
		gojatool.ToolsProviderName:     true,
		shellsession.ToolsProviderName: true,
	}
}
