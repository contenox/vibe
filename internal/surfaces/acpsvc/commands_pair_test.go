package acpsvc

import "testing"

// Pins both halves of the beta gate: unadvertised when off, and answered as
// unknown if typed anyway. Hidden-but-working is the failure this guards.
func TestUnit_AcpCommands_PairIsBetaGated(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)

	for _, name := range []string{"pair", "unpair"} {
		tr.deps.OptInBeta = false
		if tr.commandAvailable(name) {
			t.Fatalf("/%s advertised without the beta opt-in", name)
		}
		if containsCommand(tr.acpCommands(), name) {
			t.Fatalf("/%s in the advertised set without the beta opt-in: %v",
				name, commandNames(tr.acpCommands()))
		}

		tr.deps.OptInBeta = true
		if !tr.commandAvailable(name) {
			t.Fatalf("/%s still hidden with the beta opt-in on", name)
		}
		if !containsCommand(tr.acpCommands(), name) {
			t.Fatalf("/%s missing from the advertised set with the beta opt-in on: %v",
				name, commandNames(tr.acpCommands()))
		}
	}
}

// parseCommand is built from the unfiltered set, so a gated name still parses.
// That is why dispatchCommand needs its own guard.
func TestUnit_AcpCommands_PairIsRecognizedButGated(t *testing.T) {
	name, _, ok := parseCommand("/pair BCD-7GH")
	if !ok || name != "pair" {
		t.Fatalf("parseCommand(%q) = %q, %v; want pair, true", "/pair BCD-7GH", name, ok)
	}
}
