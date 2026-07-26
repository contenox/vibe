package missionservice

import (
	"encoding/json"
	"strings"
)

// MissionMetaKey is the session/new `_meta` key a dispatcher uses to tell a
// spawned unit which mission it is running. It sits beside acpsvc's
// `contenox.agent` / `contenox.adopt` keys in the same `_meta` object and is
// read the same way (a conformant ACP client that does not recognize it simply
// ignores it).
//
// This is the wire contract for the ONLY thing a dispatched unit needs in order
// to hold its mission tools: its own mission id. The tools themselves are the
// unit's own local providers (registered when it runs as `contenox acp`); what
// crosses the process boundary at session setup is just this id. Binding the id
// into the session at construction — rather than having the unit assert "I am on
// mission X" — is what makes the grant per-unit-of-work and unforgeable from the
// agent's side (see the "envelope enforced at construction" decision in
// docs/development/blueprints/acp/fleet-consolidation.md): a session that was
// not constructed with a mission id has no mission id to report against, and its
// mission tools resolve to nothing.
//
// It lives in missionservice, not acpsvc, because it is a property of the
// mission (the durable half both the dispatcher fleetservice and the unit's
// acpsvc agree on), and because the kernel that forwards it (agentinstance) may
// not import a transport. The kernel forwards an OPAQUE `_meta` blob it is
// handed (SessionSpec.Meta); only fleetservice (writer) and acpsvc (reader) know
// it carries a mission id.
const MissionMetaKey = "contenox.mission"

// MissionMeta is the value stored under MissionMetaKey.
type MissionMeta struct {
	MissionID string `json:"missionId"`
}

// MarshalMissionMeta builds the `{"contenox.mission": {"missionId": "<id>"}}`
// object a dispatcher sets on session/new so the spawned unit learns its mission
// id. Returns nil for an empty id so a non-mission session sends no `_meta` at
// all rather than an empty envelope.
func MarshalMissionMeta(missionID string) json.RawMessage {
	if strings.TrimSpace(missionID) == "" {
		return nil
	}
	raw, err := json.Marshal(map[string]MissionMeta{MissionMetaKey: {MissionID: missionID}})
	if err != nil {
		return nil
	}
	return raw
}

// ParseMissionMeta extracts the mission id from a session/new `_meta`. A missing
// key, malformed json, or an empty id all read as ("", false), so a client that
// ships unrelated `_meta` (or none) is simply not on a mission — mirroring
// acpsvc.parseAgentMeta's fail-soft contract.
func ParseMissionMeta(meta json.RawMessage) (string, bool) {
	if len(meta) == 0 {
		return "", false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(meta, &m) != nil {
		return "", false
	}
	raw, ok := m[MissionMetaKey]
	if !ok {
		return "", false
	}
	var mm MissionMeta
	if json.Unmarshal(raw, &mm) != nil {
		return "", false
	}
	id := strings.TrimSpace(mm.MissionID)
	if id == "" {
		return "", false
	}
	return id, true
}
