package contenoxcli

import (
	"context"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/internal/substrate"
	"github.com/contenox/contenox/libtracker"
)

const optInBetaKey = "opt-in-beta"

const envOptInBeta = "CONTENOX_OPT_IN_BETA"

// betaEnvOverride reads the env override; ok is false when the variable is unset
// or empty.
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

// betaEnabled reports whether beta features are visible this invocation: the env
// override wins, otherwise the stored opt-in-beta config.
func betaEnabled(ctx context.Context, store runtimetypes.Store) bool {
	if on, ok := betaEnvOverride(); ok {
		return on
	}
	return clikv.Read(ctx, store, optInBetaKey) == "true"
}

// betaEnabledGlobal resolves the gate with no command context: the env override,
// then opt-in-beta in the default global DB. An absent or unreadable DB resolves
// off.
func betaEnabledGlobal() bool {
	if on, ok := betaEnvOverride(); ok {
		return on
	}
	dbPath, err := globalDBPath()
	if err != nil {
		return false
	}
	sel, err := substrate.Resolve()
	if err != nil {
		return false
	}
	if !sel.UsesPostgres() {
		if _, err := os.Stat(dbPath); err != nil {
			// Never create the DB just to answer a visibility question.
			return false
		}
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
// invisible without the opt-in, nil when enabled.
func betaGatedToolsets(enabled bool) map[string]bool {
	if enabled {
		return nil
	}
	return map[string]bool{}
}
