package eventtrigger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/eventtrigger"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const validTrigger = `{
	"name": "on-report",
	"description": "fire a chain on every report",
	"listen_for": {"type": "missionservice.events.report_added"},
	"type": "fire_chain",
	"chain": "chain-on-report.json"
}`

func TestUnit_EventTrigger_LoadDiscoversWorkspaceOverHome(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeFile(t, workspace, "trigger-report.json", validTrigger)
	writeFile(t, home, "trigger-report.json", `{
		"name": "home-shadowed",
		"listen_for": {"type": "other.type"},
		"type": "fire_chain",
		"chain": "other.json"
	}`)
	writeFile(t, home, "trigger-home-only.json", `{
		"name": "home-only",
		"listen_for": {"type": "missionservice.events.status_changed"},
		"type": "fire_chain",
		"chain": "chain-status.json",
		"policy": "hitl-policy-default.json"
	}`)

	res, err := eventtrigger.Load(context.Background(), nil, workspace, home)
	require.NoError(t, err)
	require.Len(t, res.Triggers, 2)
	names := []string{res.Triggers[0].Name, res.Triggers[1].Name}
	require.Contains(t, names, "on-report", "workspace copy wins by basename")
	require.Contains(t, names, "home-only", "home files without a workspace shadow still load")
	require.NotContains(t, names, "home-shadowed")
	require.Empty(t, res.Skipped)
}

func TestUnit_EventTrigger_LoadSkipsMalformedAndUnknownTypeWithoutError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "trigger-good.json", validTrigger)
	writeFile(t, dir, "trigger-broken.json", `{not json`)
	writeFile(t, dir, "trigger-unknown.json", `{
		"name": "weird",
		"listen_for": {"type": "x"},
		"type": "run_webhook",
		"chain": "c.json"
	}`)
	writeFile(t, dir, "trigger-no-listen.json", `{
		"name": "deaf",
		"listen_for": {"type": ""},
		"type": "fire_chain",
		"chain": "c.json"
	}`)
	writeFile(t, dir, "not-a-trigger.json", `{"anything": true}`)

	res, err := eventtrigger.Load(context.Background(), nil, dir)
	require.NoError(t, err, "malformed files never crash the host")
	require.Len(t, res.Triggers, 1)
	require.Equal(t, "on-report", res.Triggers[0].Name)
	require.Len(t, res.Skipped, 3, "each defective trigger-* file is reported, non-trigger files are ignored")
}

func TestUnit_EventTrigger_LoadKeptRefusesEverything(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "trigger-report.json", validTrigger)

	res, err := eventtrigger.LoadKept(context.Background(), nil, func(string) bool { return false }, dir)
	require.NoError(t, err)
	require.Empty(t, res.Triggers, "a refuse-all keep loads nothing — the beta-off gate")
	require.Empty(t, res.Skipped, "kept-out triggers are not defects")
}

func TestUnit_EventTrigger_LoadSkipsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "trigger-a.json", validTrigger)
	writeFile(t, dir, "trigger-b.json", validTrigger)

	res, err := eventtrigger.Load(context.Background(), nil, dir)
	require.NoError(t, err)
	require.Len(t, res.Triggers, 1)
	require.Len(t, res.Skipped, 1)
	require.Contains(t, res.Skipped[0].Reason, "duplicate trigger name")
}

func TestUnit_EventTrigger_VetChecksShapeAndReferences(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string // "" = pass
	}{
		{"valid", validTrigger, ""},
		{"not json", `nope`, "does not parse"},
		{"no name", `{"listen_for":{"type":"x"},"type":"fire_chain","chain":"c.json"}`, "no name"},
		{"no listen type", `{"name":"n","listen_for":{"type":" "},"type":"fire_chain","chain":"c.json"}`, "listen_for.type is required"},
		{"unknown type", `{"name":"n","listen_for":{"type":"x"},"type":"nope","chain":"c.json"}`, `unknown type "nope"`},
		{"no chain", `{"name":"n","listen_for":{"type":"x"},"type":"fire_chain","chain":""}`, "chain is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := eventtrigger.Vet([]byte(tc.data), nil)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}

	// Reference resolution: the chain must resolve; a named policy must too.
	missing := errors.New("file not found")
	resolve := func(name string) error {
		if name == "chain-on-report.json" {
			return nil
		}
		return missing
	}
	require.NoError(t, eventtrigger.Vet([]byte(validTrigger), resolve))
	withPolicy := `{
		"name": "n",
		"listen_for": {"type": "x"},
		"type": "fire_chain",
		"chain": "chain-on-report.json",
		"policy": "hitl-policy-nope.json"
	}`
	require.ErrorIs(t, eventtrigger.Vet([]byte(withPolicy), resolve), missing)
}

func TestUnit_EventTrigger_IsTriggerFile(t *testing.T) {
	require.True(t, eventtrigger.IsTriggerFile("trigger-x.json"))
	require.True(t, eventtrigger.IsTriggerFile("Trigger-X.JSON"))
	require.False(t, eventtrigger.IsTriggerFile("trigger-x.yaml"))
	require.False(t, eventtrigger.IsTriggerFile("chain-x.json"))
}
