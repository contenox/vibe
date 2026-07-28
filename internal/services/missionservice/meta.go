package missionservice

import (
	"encoding/json"
	"strings"
)

// MissionMetaKey is the session/new `_meta` key a dispatcher uses to tell a
// spawned unit which mission it is running. Binding the id
// into the session at construction, rather than trusting the unit to assert
// it, makes the grant per-unit-of-work and unforgeable from the agent's
// side. A conformant ACP client that doesn't recognize the key ignores it.
const MissionMetaKey = "contenox.mission"

// MissionMeta is the value stored under MissionMetaKey. ModelAllowlist and
// BackendAllowlist are a bound, not a grant: they ride in at session setup
// because the supervising process cannot enforce model choice from outside,
// so the unit enforces the bound against its own resolver
// (llmrepo.WithResolutionBounds). The direction is narrowing-only — `_meta`
// a unit doesn't receive leaves it exactly as unbounded as today.
type MissionMeta struct {
	MissionID string `json:"missionId"`
	// Omitted (nil) means unbounded for that dimension.
	ModelAllowlist   []string `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string `json:"backendAllowlist,omitempty"`
}

// MarshalMissionMeta builds the `{"contenox.mission": {"missionId": "<id>"}}`
// object a dispatcher sets on session/new. Returns nil for an empty id so a
// non-mission session sends no `_meta` at all.
func MarshalMissionMeta(missionID string) json.RawMessage {
	return MarshalMissionMetaBounded(missionID, nil, nil)
}

// MarshalMissionMetaBounded is MarshalMissionMeta plus the envelope's
// model/backend allowlists. Empty lists marshal away entirely (omitempty),
// so a mission with no allowlist produces the same `_meta` as before this existed.
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

// trimmedNonEmpty drops blank entries and trims the rest. Returns nil (not
// an empty slice) when nothing survives, keeping the field off the wire.
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

// ParseMissionMeta extracts the mission id from a session/new `_meta`. A
// missing key, malformed json, or empty id all read as ("", false) — fail-soft,
// so a client shipping unrelated or no `_meta` is simply not on a mission.
func ParseMissionMeta(meta json.RawMessage) (string, bool) {
	mm, ok := ParseMissionMetaFull(meta)
	return mm.MissionID, ok
}

// ParseMissionMetaFull is ParseMissionMeta plus the envelope bounds. Same
// fail-soft contract; a `_meta` carrying only an id yields nil allowlists.
// The returned MissionID and allowlists are trimmed and blank-free.
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
