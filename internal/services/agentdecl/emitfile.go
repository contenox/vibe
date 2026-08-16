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
var ReservedNames = map[string]bool{
	"agent-planner": true,
}

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
