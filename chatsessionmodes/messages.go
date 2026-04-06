package chatsessionmodes

import (
	"github.com/contenox/contenox/taskengine"
)

// PrependInjectionsBeforeLastUser splices injected system messages immediately before the last message (the new user turn).
func PrependInjectionsBeforeLastUser(thread []taskengine.Message, injected []taskengine.Message) ([]taskengine.Message, error) {
	if len(injected) == 0 {
		return thread, nil
	}
	if len(thread) == 0 {
		return nil, errEmptyThread
	}
	n := len(thread)
	prior := thread[:n-1]
	last := thread[n-1]
	if last.Role != "user" {
		return nil, errLastNotUser
	}
	out := make([]taskengine.Message, 0, len(prior)+len(injected)+1)
	out = append(out, prior...)
	out = append(out, injected...)
	out = append(out, last)
	return out, nil
}

// MergeChatHistoryPreservingInjections ensures injected system messages appear in the persisted thread
// when the chain output dropped them.
func MergeChatHistoryPreservingInjections(injected []taskengine.Message, out []taskengine.Message) []taskengine.Message {
	if len(injected) == 0 || len(out) == 0 {
		return out
	}
	present := make(map[string]struct{}, len(out))
	for _, m := range out {
		if m.ID != "" {
			present[m.ID] = struct{}{}
		}
	}
	allPresent := true
	for _, m := range injected {
		if m.ID == "" {
			allPresent = false
			break
		}
		if _, ok := present[m.ID]; !ok {
			allPresent = false
			break
		}
	}
	if allPresent {
		return out
	}
	lastUser := -1
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return out
	}
	merged := make([]taskengine.Message, 0, len(out)+len(injected))
	merged = append(merged, out[:lastUser]...)
	merged = append(merged, injected...)
	merged = append(merged, out[lastUser:]...)
	return merged
}
