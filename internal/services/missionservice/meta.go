package missionservice

import (
	"encoding/json"
	"strings"
)

// MissionMetaKey is the session/new `_meta` key a dispatcher uses to tell a spawned unit which mission it is running; a conformant ACP client that doesn't recognize the key ignores it.
const MissionMetaKey = "contenox.mission"

// MissionMeta is the value stored under MissionMetaKey; ModelAllowlist and BackendAllowlist are a bound, not a grant, enforced by the unit against its own resolver, and narrowing-only.
type MissionMeta struct {
	MissionID string `json:"missionId"`
	// Omitted (nil) means unbounded for that dimension.
	ModelAllowlist   []string `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string `json:"backendAllowlist,omitempty"`
}

// MarshalMissionMeta builds the `{"contenox.mission": {"missionId": "<id>"}}` object a dispatcher sets on session/new, returning nil for an empty id so a non-mission session sends no `_meta` at all.
func MarshalMissionMeta(missionID string) json.RawMessage {
	return MarshalMissionMetaBounded(missionID, nil, nil)
}

// MarshalMissionMetaBounded is MarshalMissionMeta plus the envelope's model/backend allowlists; empty lists marshal away entirely (omitempty).
func MarshalMissionMetaBounded(missionID string, modelAllowlist, backendAllowlist []string) json.RawMessage {
	if strings.TrimSpace(missionID) == "" {
		return nil
	}
	meta := MissionMeta{
		MissionID:        missionID,
		ModelAllowlist:   trimmedNonEmpty(modelAllowlist),
		BackendAllowlist: trimmedNonEmpty(backendAllowlist),
	}
	raw, err := json.Marshal(map[string]MissionMeta{MissionMetaKey: meta})
	if err != nil {
		return nil
	}
	return raw
}

func trimmedNonEmpty(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if t := strings.TrimSpace(e); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseMissionMeta extracts the mission id from a session/new `_meta`; a missing key, malformed json, or empty id all read as ("", false), fail-soft.
func ParseMissionMeta(meta json.RawMessage) (string, bool) {
	mm, ok := ParseMissionMetaFull(meta)
	return mm.MissionID, ok
}

// ParseMissionMetaFull is ParseMissionMeta plus the envelope bounds, same fail-soft contract, returning a trimmed, blank-free MissionID and allowlists.
func ParseMissionMetaFull(meta json.RawMessage) (MissionMeta, bool) {
	if len(meta) == 0 {
		return MissionMeta{}, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return MissionMeta{}, false
	}
	raw, ok := m[MissionMetaKey]
	if !ok {
		return MissionMeta{}, false
	}
	var mm MissionMeta
	if json.Unmarshal(raw, &mm) != nil {
		return MissionMeta{}, false
	}
	id := strings.TrimSpace(mm.MissionID)
	if id == "" {
		return MissionMeta{}, false
	}
	return MissionMeta{
		MissionID:        id,
		ModelAllowlist:   trimmedNonEmpty(mm.ModelAllowlist),
		BackendAllowlist: trimmedNonEmpty(mm.BackendAllowlist),
	}, true
}
