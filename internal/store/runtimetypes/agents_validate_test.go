package runtimetypes_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func absTestPath(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	return `C:` + filepath.FromSlash(p)
}

func TestUnit_ExternalACPConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     runtimetypes.ExternalACPConfig
		wantErr bool
	}{
		{
			name: "stdio with command is valid",
			cfg: runtimetypes.ExternalACPConfig{
				Transport: runtimetypes.ExternalACPTransportStdio,
				Command:   "my-acp-agent",
			},
			wantErr: false,
		},
		{
			name: "stdio without command is invalid",
			cfg: runtimetypes.ExternalACPConfig{
				Transport: runtimetypes.ExternalACPTransportStdio,
			},
			wantErr: true,
		},
		{
			name: "endpoint with url is valid",
			cfg: runtimetypes.ExternalACPConfig{
				Transport: runtimetypes.ExternalACPTransportEndpoint,
				URL:       "https://agent.example.com/acp",
			},
			wantErr: false,
		},
		{
			name: "endpoint without url is invalid",
			cfg: runtimetypes.ExternalACPConfig{
				Transport: runtimetypes.ExternalACPTransportEndpoint,
			},
			wantErr: true,
		},
		{
			name:    "empty transport is invalid",
			cfg:     runtimetypes.ExternalACPConfig{},
			wantErr: true,
		},
		{
			name: "unknown transport is invalid",
			cfg: runtimetypes.ExternalACPConfig{
				Transport: "carrier-pigeon",
				Command:   "irrelevant",
				URL:       "irrelevant",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUnit_ChainConfig_Validate(t *testing.T) {
	abs := absTestPath(filepath.Join(string(filepath.Separator), "home", "u", ".contenox", "agent-reviewer.json"))
	tests := []struct {
		name    string
		cfg     runtimetypes.ChainConfig
		wantErr bool
	}{
		{
			name:    "absolute path is valid",
			cfg:     runtimetypes.ChainConfig{Path: abs, ChainID: "agent-reviewer"},
			wantErr: false,
		},
		{
			name:    "chain id is optional",
			cfg:     runtimetypes.ChainConfig{Path: abs},
			wantErr: false,
		},
		{
			name:    "empty path is invalid",
			cfg:     runtimetypes.ChainConfig{},
			wantErr: true,
		},
		{
			name:    "blank path is invalid",
			cfg:     runtimetypes.ChainConfig{Path: "   "},
			wantErr: true,
		},
		{
			name:    "relative path is invalid",
			cfg:     runtimetypes.ChainConfig{Path: filepath.Join("chains", "agent-reviewer.json")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestUnit_Agents_ChainConfig_Accessors verifies Set stamps the kind, Get refuses a record of a different kind, and an empty ConfigJSON reads as a zero config.
func TestUnit_Agents_ChainConfig_Accessors(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "chains", "agent-reviewer.json")

	agent := &runtimetypes.Agent{Name: "reviewer"}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{Path: path, ChainID: "agent-reviewer"}))
	assert.Equal(t, runtimetypes.AgentKindChain, agent.Kind, "Set stamps the kind, so the two can never disagree")

	cfg, err := agent.ChainConfig()
	require.NoError(t, err)
	assert.Equal(t, path, cfg.Path)
	assert.Equal(t, "agent-reviewer", cfg.ChainID)

	external := &runtimetypes.Agent{Name: "ext"}
	require.NoError(t, external.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "some-agent",
	}))
	_, err = external.ChainConfig()
	require.Error(t, err)

	bare := &runtimetypes.Agent{Name: "bare", Kind: runtimetypes.AgentKindChain}
	cfg, err = bare.ChainConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Path)
}
