package acpsvc

import "testing"

// Pins /pair and /unpair as stable: advertised and dispatchable regardless of
// the beta opt-in. They were beta-gated until the relay path was proven end to
// end; a regression that re-hides them would strand paired machines behind a
// flag their operators never set.
func TestUnit_AcpCommands_PairIsStable(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)

	for _, name := range []string{"pair", "unpair"} {
		for _, beta := range []bool{false, true} {
			tr.deps.OptInBeta = beta
			if !tr.commandAvailable(name) {
				t.Fatalf("/%s hidden with OptInBeta=%v", name, beta)
			}
			if !containsCommand(tr.acpCommands(), name) {
				t.Fatalf("/%s missing from the advertised set with OptInBeta=%v: %v",
					name, beta, commandNames(tr.acpCommands()))
			}
		}
	}
}

// parseCommand is built from the unfiltered set, so /pair parses as a command
// rather than falling through to the model as prompt text.
func TestUnit_AcpCommands_PairIsRecognized(t *testing.T) {
	name, _, ok := parseCommand("/pair BCD-7GH")
	if !ok || name != "pair" {
		t.Fatalf("parseCommand(%q) = %q, %v; want pair, true", "/pair BCD-7GH", name, ok)
	}
}
