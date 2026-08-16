package agentdecl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WriteAction is what one pass did with one declaration.
type WriteAction string

const (
	ActionCreated   WriteAction = "created"
	ActionUpdated   WriteAction = "updated"
	ActionUnchanged WriteAction = "unchanged"
	ActionRefused   WriteAction = "refused"
	// ActionIgnored is configuration that named something that does not exist.
	// Not a refusal: nothing was rejected, a knob simply had nothing to act on.
	ActionIgnored WriteAction = "ignored"
)

// ReservedNames are the shipped agents a declaration may not take the id of.
// The workspace directory is the first root chainagents scans, so a declaration
// claiming one would shadow the shipped agent rather than error.
//
// ⚠ chain-contenox, chain-acp, chain-acpx, chain-beam and chain-run are NOT
// here any more. They stopped being shipped JSON and became the seeded
// declarations under agents/, so reserving their names would refuse the very
// files init writes. They are ordinary declarations now, and a workspace copy
// shadowing one is the same "your copy wins" rule every chain already follows.
var ReservedNames = map[string]bool{
	"agent-planner": true,
}

// marshalWithSchema splices the $schema key in ahead of the marshalled body so
// an editor completes and checks an emitted file the way it does a shipped one.
func marshalWithSchema(v any, schemaURL string) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(body), "{\n") {
		return body, nil
	}
	head := fmt.Sprintf("{\n  \"$schema\": %q,\n", schemaURL)
	return append([]byte(head), body[2:]...), nil
}
