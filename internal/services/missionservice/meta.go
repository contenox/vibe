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
//
// ModelAllowlist / BackendAllowlist are the second thing that must cross the
// process boundary, and for a different reason than the mission id. The id is a
// GRANT (it hands the unit its mission tools); these are a BOUND (they take model
// choice away). They ride here because the supervising process cannot enforce
// them from outside: a dispatched unit resolves its own models in its own
// process, and the ACP session/update contract carries no model identity for the
// host to watch. So the bound travels IN at session setup and is enforced by the
// unit against its own resolver (llmrepo.WithResolutionBounds), where the choice
// is actually made.
//
// That the unit enforces its own bound is safe for the same structural reason the
// mission-id grant is: a unit is this runtime re-invoked as an ACP peer over
// stdio (agentinstance's chain branch — the only user-declarable agent kind), not
// a foreign process being asked to police itself. And the direction is
// narrowing-only: `_meta` a unit does not receive leaves it exactly as unbounded
// as it is today, so a dropped or ignored bound can never GRANT anything.
type MissionMeta struct {
	MissionID string `json:"missionId"`
	// ModelAllowlist / BackendAllowlist mirror hitlservice.ComputeBounds. Omitted
	// (nil) means unbounded for that dimension, which is what every envelope
	// without a compute block sends.
	ModelAllowlist   []string `json:"modelAllowlist,omitempty"`
	BackendAllowlist []string `json:"backendAllowlist,omitempty"`
}

// MarshalMissionMeta builds the `{"contenox.mission": {"missionId": "<id>"}}`
// object a dispatcher sets on session/new so the spawned unit learns its mission
// id. Returns nil for an empty id so a non-mission session sends no `_meta` at
// all rather than an empty envelope.
func MarshalMissionMeta(missionID string) json.RawMessage {
	return MarshalMissionMetaBounded(missionID, nil, nil)
}

// MarshalMissionMetaBounded is MarshalMissionMeta plus the envelope's
// model/backend allowlists, for a dispatcher that read the mission's compute
// bounds. Empty lists marshal away entirely (the `omitempty` tags), so a mission
// with no allowlist produces byte-for-byte the same `_meta` as before this
// existed.
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

// trimmedNonEmpty drops blank entries and trims the rest, so a hand-edited
// envelope's stray whitespace cannot produce an allowlist entry that matches
// nothing. Returns nil (not an empty slice) when nothing survives, which is what
// keeps the field off the wire.
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

// ParseMissionMeta extracts the mission id from a session/new `_meta`. A missing
// key, malformed json, or an empty id all read as ("", false), so a client that
// ships unrelated `_meta` (or none) is simply not on a mission — mirroring
// acpsvc.parseAgentMeta's fail-soft contract.
func ParseMissionMeta(meta json.RawMessage) (string, bool) {
	mm, ok := ParseMissionMetaFull(meta)
	return mm.MissionID, ok
}

// ParseMissionMetaFull is ParseMissionMeta plus the envelope bounds the
// dispatcher attached. Same fail-soft contract: anything unparseable reads as
// "not on a mission" rather than an error, and a `_meta` carrying only an id
// yields nil allowlists — unbounded, today's behavior.
//
// The returned MissionID is trimmed; the allowlists are trimmed and blank-free,
// so the reader can compare them without re-cleaning.
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
