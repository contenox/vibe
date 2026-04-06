package chatsessionmodes

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultChainByMode maps product mode ids to VFS chain references when ?chainId is omitted.
// chat: conversational default; prompt: same file until a dedicated prompt chain ships; plan: same (plan tools via plan_manager in chain JSON).
var DefaultChainByMode = map[string]string{
	"chat":   "default-chain.json",
	"prompt": "default-chain.json",
	"plan":   "default-chain.json",
}

// ChainResolver resolves which chain ref to load given explicit query and mode.
type ChainResolver interface {
	Resolve(explicitChainID, mode string) (chainRef string, err error)
}

// MapChainResolver uses DefaultChainByMode (or a copy passed at construction).
type MapChainResolver struct {
	ModeToChain map[string]string
}

// Resolve implements ChainResolver.
func (m *MapChainResolver) Resolve(explicitChainID, mode string) (string, error) {
	q := strings.TrimSpace(explicitChainID)
	if q != "" {
		return q, nil
	}
	mod := strings.TrimSpace(strings.ToLower(mode))
	if mod == "" {
		return "", errors.New("chainId query parameter or non-empty mode in body is required")
	}
	ref, ok := m.ModeToChain[mod]
	if !ok {
		return "", fmt.Errorf("unknown chat mode %q", strings.TrimSpace(mode))
	}
	return ref, nil
}
