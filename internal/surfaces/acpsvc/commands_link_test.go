package acpsvc

import (
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
)

// Pins /link to /pair's availability shape: advertised and dispatchable
// regardless of the beta opt-in.
func TestUnit_AcpCommands_LinkIsStable(t *testing.T) {
	tr, _ := newMissionTestTransport(t, nil, nil)

	for _, beta := range []bool{false, true} {
		tr.deps.OptInBeta = beta
		if !tr.commandAvailable("link") {
			t.Fatalf("/link hidden with OptInBeta=%v", beta)
		}
		if !containsCommand(tr.acpCommands(), "link") {
			t.Fatalf("/link missing from the advertised set with OptInBeta=%v: %v",
				beta, commandNames(tr.acpCommands()))
		}
	}
}

// parseCommand is built from the unfiltered set, so /link parses as a command
// rather than falling through to the model as prompt text.
func TestUnit_AcpCommands_LinkIsRecognized(t *testing.T) {
	name, _, ok := parseCommand("/link")
	if !ok || name != "link" {
		t.Fatalf("parseCommand(%q) = %q, %v; want link, true", "/link", name, ok)
	}
}

// Paired against the hosted relay: the deep link uses the app's canonical
// hostname (relaypair.DefaultAppEndpoint), never the machine dial address —
// the hosted relay and app are one deployment under two hostnames, and links
// shown to people use the app's.
func TestUnit_HandleLink_HostedPairingLinksAppHostname(t *testing.T) {
	dir := t.TempDir()
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:       relaypair.DefaultEndpoint,
		InstanceToken:  "secret-token",
		InstanceID:     "inst-42",
		AccountID:      "acct-1",
		RelayPublicKey: "irrelevant-here",
	}); err != nil {
		t.Fatalf("seed pairing: %v", err)
	}
	tr := &Transport{deps: Deps{ContenoxDir: dir}}

	out, err := tr.handleLink("zed-0b8a4d55")
	if err != nil {
		t.Fatalf("handleLink: %v", err)
	}
	want := "https://app.contenox.com/session/inst-42/zed-0b8a4d55\n" +
		"Open this on your phone to attach to this session — sign-in required."
	if out != want {
		t.Fatalf("handleLink output:\n%q\nwant:\n%q", out, want)
	}
	if strings.Contains(out, "secret-token") {
		t.Fatalf("handleLink leaked the instance token: %q", out)
	}
}

// Paired against a self-hosted relay: the deep link keeps the endpoint's own
// origin — a self-hosted relay serves the app same-origin — plus the app's
// attached route, /session/<instance id>/<session id>, and one plain line.
func TestUnit_HandleLink_SelfHostedPairingKeepsEndpointOrigin(t *testing.T) {
	dir := t.TempDir()
	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:       "https://relay.example.com",
		InstanceToken:  "secret-token",
		InstanceID:     "inst-42",
		AccountID:      "acct-1",
		RelayPublicKey: "irrelevant-here",
	}); err != nil {
		t.Fatalf("seed pairing: %v", err)
	}
	tr := &Transport{deps: Deps{ContenoxDir: dir}}

	out, err := tr.handleLink("zed-0b8a4d55")
	if err != nil {
		t.Fatalf("handleLink: %v", err)
	}
	want := "https://relay.example.com/session/inst-42/zed-0b8a4d55\n" +
		"Open this on your phone to attach to this session — sign-in required."
	if out != want {
		t.Fatalf("handleLink output:\n%q\nwant:\n%q", out, want)
	}
}

// Unpaired: a teaching nudge toward /pair, not an error — mirroring
// pairingStatus's unpaired answer.
func TestUnit_HandleLink_UnpairedNudgesPair(t *testing.T) {
	tr := &Transport{deps: Deps{ContenoxDir: t.TempDir()}}

	out, err := tr.handleLink("zed-0b8a4d55")
	if err != nil {
		t.Fatalf("handleLink on an unpaired machine: %v", err)
	}
	if !strings.Contains(out, "not paired") || !strings.Contains(out, "/pair") {
		t.Fatalf("unpaired answer must nudge toward /pair, got: %q", out)
	}
	if strings.Contains(out, "/session/") {
		t.Fatalf("unpaired answer must not contain a link, got: %q", out)
	}
}
