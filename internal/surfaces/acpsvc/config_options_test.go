package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

func setupConfigOptionsDB(t *testing.T) (context.Context, libdb.DBManager) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "acp-config-options.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return ctx, db
}

func TestUnit_SessionConfigOptionsExposeModelPolicyAndThink(t *testing.T) {
	ctx, db := setupConfigOptionsDB(t)
	// A decoy global value: per-session display must ignore the global KV entirely
	// (the HITL policy is session-scoped, like model/think/token-limit).
	require.NoError(t, clikv.SetHITLPolicy(ctx, runtimetypes.New(db.WithoutTransaction()), "strict"))

	tr := &Transport{
		deps: Deps{
			DB:                    db,
			KnownPolicies:         []string{"strict", "dev"},
			HITLDefaultPolicyName: "strict",
		},
		defaultProvider:    "openai",
		defaultModel:       "gpt-5-mini",
		defaultAltProvider: "anthropic",
		defaultAltModel:    "claude-sonnet-4",
	}
	sess := &sessionEntry{Think: "medium", HITLPolicy: "dev", driver: &nativeDriver{t: tr}}

	options := tr.sessionConfigOptions(ctx, sess)
	require.Len(t, options, 4)

	model := optionByID(t, options, configIDModel)
	require.Equal(t, configCategoryModel, model.Category)
	require.Equal(t, "openai/gpt-5-mini", model.CurrentValue)
	require.Len(t, model.Options.Groups, 2)
	require.Equal(t, "openai", model.Options.Groups[0].Group)
	require.Equal(t, "gpt-5-mini", model.Options.Groups[0].Options[0].Name)
	require.Equal(t, "anthropic", model.Options.Groups[1].Group)
	require.Equal(t, "claude-sonnet-4", model.Options.Groups[1].Options[0].Name)
	require.True(t, configOptionHasValue(model, "openai/gpt-5-mini"))
	require.True(t, configOptionHasValue(model, "anthropic/claude-sonnet-4"))

	policy := optionByID(t, options, configIDHITLPolicy)
	require.Equal(t, configCategoryHITLPolicy, policy.Category)
	require.Equal(t, "dev", policy.CurrentValue, "per-session HITL policy drives CurrentValue, not the global cli.hitl-policy-name KV")
	require.True(t, configOptionHasValue(policy, hitlPolicyDefaultValue))
	require.True(t, configOptionHasValue(policy, "strict"))
	require.True(t, configOptionHasValue(policy, "dev"))

	think := optionByID(t, options, configIDThink)
	require.Equal(t, configCategoryThink, think.Category)
	require.Equal(t, "medium", think.CurrentValue)
	require.True(t, configOptionHasValue(think, "auto"))
	require.True(t, configOptionHasValue(think, "off"))
	require.True(t, configOptionHasValue(think, "xhigh"))

	limit := optionByID(t, options, configIDTokenLimit)
	require.Equal(t, "context", limit.Category)
	require.Equal(t, "0", limit.CurrentValue) // default
	require.Contains(t, limit.Description, "token limit")
}

func TestUnit_HITLPolicyDisplayNameShortensPresetFilenames(t *testing.T) {
	require.Equal(t, "strict", hitlPolicyDisplayName("hitl-policy-strict.json"))
	require.Equal(t, "acpx", hitlPolicyDisplayName("hitl-policy-acpx.json"))
	require.Equal(t, "custom", hitlPolicyDisplayName("custom.json"))
}

func TestUnit_ModelConfigDisplayNameStripsGeminiResourcePrefix(t *testing.T) {
	require.Equal(t, "gemini-3.1-pro-preview", modelConfigDisplayName("models/gemini-3.1-pro-preview"))
	require.Equal(t, "gpt-5", modelConfigDisplayName("gpt-5"))
}

func TestUnit_SetSessionConfigOptionUpdatesSessionAndPolicyConfig(t *testing.T) {
	ctx, db := setupConfigOptionsDB(t)

	sid := libacp.SessionID("sess-1")
	sess := &sessionEntry{Provider: "openai", Model: "gpt-5-mini", Think: "low"}
	tr := &Transport{
		deps: Deps{
			DB:                    db,
			KnownPolicies:         []string{"strict", "dev"},
			HITLDefaultPolicyName: "strict",
		},
		sessions:           map[libacp.SessionID]*sessionEntry{sid: sess},
		contenoxToACPID:    map[string]libacp.SessionID{},
		defaultProvider:    "openai",
		defaultModel:       "gpt-5-mini",
		defaultAltProvider: "anthropic",
		defaultAltModel:    "claude-sonnet-4",
	}
	sess.driver = &nativeDriver{t: tr}

	resp, err := tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sid,
		ConfigID:  configIDModel,
		Value:     libacp.StringConfigValue("anthropic/claude-sonnet-4"),
	})
	require.NoError(t, err)
	require.Equal(t, "openai", tr.provider(), "ACP selector changes the session, not the persistent default provider")
	require.Equal(t, "gpt-5-mini", tr.model(), "ACP selector changes the session, not the persistent default model")
	require.Empty(t, ReadConfigValue(ctx, db, "default-provider"))
	require.Empty(t, ReadConfigValue(ctx, db, "default-model"))
	require.Equal(t, "anthropic", sess.providerOrDefault(""))
	require.Equal(t, "claude-sonnet-4", sess.modelOrDefault(""))
	require.Equal(t, "anthropic/claude-sonnet-4", optionByID(t, resp.ConfigOptions, configIDModel).CurrentValue)

	resp, err = tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sid,
		ConfigID:  configIDHITLPolicy,
		Value:     libacp.StringConfigValue("dev"),
	})
	require.NoError(t, err)
	require.Equal(t, "dev", sess.hitlPolicy(), "HITL policy is stored on the session")
	require.Empty(t, clikv.ReadHITLPolicy(ctx, runtimetypes.New(db.WithoutTransaction())),
		"setting the toolbar HITL policy must NOT write the global cli.hitl-policy-name KV")
	require.Equal(t, "dev", optionByID(t, resp.ConfigOptions, configIDHITLPolicy).CurrentValue)
	require.Equal(t, "dev", tr.resolveSessionHITLPolicy(sess), "a concrete selection resolves to its own name for enforcement")

	resp, err = tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sid,
		ConfigID:  configIDHITLPolicy,
		Value:     libacp.StringConfigValue(hitlPolicyDefaultValue),
	})
	require.NoError(t, err)
	require.Equal(t, hitlPolicyDefaultValue, sess.hitlPolicy())
	require.Empty(t, clikv.ReadHITLPolicy(ctx, runtimetypes.New(db.WithoutTransaction())))
	require.Equal(t, hitlPolicyDefaultValue, optionByID(t, resp.ConfigOptions, configIDHITLPolicy).CurrentValue)
	require.Equal(t, "strict", tr.resolveSessionHITLPolicy(sess), "the sentinel resolves to the operator-configured default policy")

	resp, err = tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sid,
		ConfigID:  configIDThink,
		Value:     libacp.StringConfigValue("xhigh"),
	})
	require.NoError(t, err)
	require.Equal(t, "xhigh", sess.think())
	require.Equal(t, "xhigh", optionByID(t, resp.ConfigOptions, configIDThink).CurrentValue)
}

// TestUnit_HITLPolicyIsPerSessionIndependent proves the fix's core invariant:
// two live sessions on ONE transport carry independent HITL policy selections,
// each drives its own config-option CurrentValue and its own enforcement policy
// name (resolveSessionHITLPolicy — what prompt.go injects into the turn context),
// and NEITHER write touches the global cli.hitl-policy-name KV. Together with the
// hitlservice ctx-override tests this is the end-to-end guarantee that two
// concurrent ACP sessions gate independently through the shared engine.
func TestUnit_HITLPolicyIsPerSessionIndependent(t *testing.T) {
	ctx, db := setupConfigOptionsDB(t)

	sidA := libacp.SessionID("sess-A")
	sidB := libacp.SessionID("sess-B")
	sessA := &sessionEntry{Provider: "openai", Model: "gpt-5-mini", Think: "low", HITLPolicy: hitlPolicyDefaultValue}
	sessB := &sessionEntry{Provider: "openai", Model: "gpt-5-mini", Think: "low", HITLPolicy: hitlPolicyDefaultValue}
	tr := &Transport{
		deps: Deps{
			DB:                    db,
			KnownPolicies:         []string{"strict", "dev"},
			HITLDefaultPolicyName: "strict",
		},
		sessions:        map[libacp.SessionID]*sessionEntry{sidA: sessA, sidB: sessB},
		contenoxToACPID: map[string]libacp.SessionID{},
		defaultProvider: "openai",
		defaultModel:    "gpt-5-mini",
	}
	sessA.driver = &nativeDriver{t: tr}
	sessB.driver = &nativeDriver{t: tr}

	// Session A picks the permissive-in-name "dev" policy; session B stays on the
	// configured default (sentinel).
	_, err := tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sidA, ConfigID: configIDHITLPolicy, Value: libacp.StringConfigValue("dev"),
	})
	require.NoError(t, err)
	_, err = tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sidB, ConfigID: configIDHITLPolicy, Value: libacp.StringConfigValue("strict"),
	})
	require.NoError(t, err)

	// Independent per-session state.
	require.Equal(t, "dev", sessA.hitlPolicy())
	require.Equal(t, "strict", sessB.hitlPolicy())

	// Independent enforcement policy names (what prompt.go injects per turn).
	require.Equal(t, "dev", tr.resolveSessionHITLPolicy(sessA))
	require.Equal(t, "strict", tr.resolveSessionHITLPolicy(sessB))

	// Independent config-option display.
	require.Equal(t, "dev", optionByID(t, tr.sessionConfigOptions(ctx, sessA), configIDHITLPolicy).CurrentValue)
	require.Equal(t, "strict", optionByID(t, tr.sessionConfigOptions(ctx, sessB), configIDHITLPolicy).CurrentValue)

	// The global KV was never written by either session.
	require.Empty(t, clikv.ReadHITLPolicy(ctx, runtimetypes.New(db.WithoutTransaction())),
		"per-session HITL selection must never write the global cli.hitl-policy-name KV")
}

func TestUnit_SetSessionConfigOptionRejectsUnknownValue(t *testing.T) {
	ctx, db := setupConfigOptionsDB(t)

	sid := libacp.SessionID("sess-1")
	sess := &sessionEntry{Provider: "openai", Model: "gpt-5-mini", Think: "low"}
	tr := &Transport{
		deps:               Deps{DB: db},
		sessions:           map[libacp.SessionID]*sessionEntry{sid: sess},
		defaultProvider:    "openai",
		defaultModel:       "gpt-5-mini",
		defaultAltProvider: "anthropic",
		defaultAltModel:    "claude-sonnet-4",
	}
	sess.driver = &nativeDriver{t: tr}

	_, err := tr.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: sid,
		ConfigID:  configIDModel,
		Value:     libacp.StringConfigValue("openai/not-advertised"),
	})
	require.Error(t, err)
	var rpcErr *libacp.Error
	require.ErrorAs(t, err, &rpcErr)
	require.Equal(t, libacp.ErrInvalidParams, rpcErr.Code)
	require.Equal(t, "openai", tr.provider())
	require.Equal(t, "gpt-5-mini", tr.model())
}

// TestUnit_WorkspaceConfigOptionsMirrorFreshSession pins the session-less
// snapshot advertised at initialize time: it must carry the same four options
// (model/HITL/think/token-limit), seeded from the transport defaults, that a
// freshly-minted session carries — so the empty-chat controls a client renders
// from _meta match what the first session/new returns.
func TestUnit_WorkspaceConfigOptionsMirrorFreshSession(t *testing.T) {
	ctx, db := setupConfigOptionsDB(t)

	tr := &Transport{
		deps: Deps{
			DB:                    db,
			KnownPolicies:         []string{"strict", "dev"},
			HITLDefaultPolicyName: "strict",
		},
		defaultProvider:    "openai",
		defaultModel:       "gpt-5-mini",
		defaultAltProvider: "anthropic",
		defaultAltModel:    "claude-sonnet-4",
		defaultThink:       "medium",
	}

	options := tr.workspaceConfigOptions(ctx)
	require.Len(t, options, 4)

	model := optionByID(t, options, configIDModel)
	require.Equal(t, "openai/gpt-5-mini", model.CurrentValue)
	require.True(t, configOptionHasValue(model, "openai/gpt-5-mini"))
	require.True(t, configOptionHasValue(model, "anthropic/claude-sonnet-4"))

	think := optionByID(t, options, configIDThink)
	require.Equal(t, "medium", think.CurrentValue, "workspace think must reflect thinkDefault(), not the bare accessor fallback")

	policy := optionByID(t, options, configIDHITLPolicy)
	require.True(t, configOptionHasValue(policy, hitlPolicyDefaultValue))

	limit := optionByID(t, options, configIDTokenLimit)
	require.Equal(t, "0", limit.CurrentValue)

	// Byte-identical to what the first session (seeded from the same defaults)
	// carries — the single source of truth for the option shapes.
	sess := &sessionEntry{Provider: tr.provider(), Model: tr.model(), Think: tr.thinkDefault(), driver: &nativeDriver{t: tr}}
	require.Equal(t, tr.sessionConfigOptions(ctx, sess), options)
}

func optionByID(t *testing.T, options []libacp.SessionConfigOption, id string) libacp.SessionConfigOption {
	t.Helper()
	for _, option := range options {
		if option.ID == id {
			return option
		}
	}
	t.Fatalf("option %q not found in %#v", id, options)
	return libacp.SessionConfigOption{}
}
