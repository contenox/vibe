package backendapi

import (
	"sort"
	"strings"

	"github.com/contenox/contenox/statetype"
)

func sanitizeRuntimeStates(states []statetype.BackendRuntimeState) []statetype.BackendRuntimeState {
	if len(states) == 0 {
		return nil
	}

	sanitized := make([]statetype.BackendRuntimeState, 0, len(states))
	for _, state := range states {
		state.Models = observedModelNames(state)
		sanitized = append(sanitized, state)
	}
	return sanitized
}

func observedModelNames(state statetype.BackendRuntimeState) []string {
	names := make([]string, 0, len(state.PulledModels))
	seen := make(map[string]struct{}, len(state.PulledModels))
	for _, model := range state.PulledModels {
		name := strings.TrimSpace(model.Model)
		if name == "" {
			name = strings.TrimSpace(model.Name)
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	sort.Strings(names)
	return names
}
