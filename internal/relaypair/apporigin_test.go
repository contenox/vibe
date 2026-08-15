package relaypair_test

import (
	"testing"

	"github.com/contenox/contenox/internal/relaypair"
)

// The hosted relay and the hosted app are different hostnames, so the relay's
// endpoint is not a URL a person can open. Only that pair is special-cased.
func TestUnit_Relaypair_HostedRelayMapsToTheAppOrigin(t *testing.T) {
	got, err := relaypair.AppOrigin(relaypair.DefaultEndpoint)
	if err != nil {
		t.Fatalf("AppOrigin: %v", err)
	}
	if got != relaypair.DefaultAppEndpoint {
		t.Fatalf("AppOrigin(hosted) = %q, want %q", got, relaypair.DefaultAppEndpoint)
	}
}

// Self-hosting is the same mechanism, not a second one: a relay on another
// domain — with its own public key, verified at redemption — serves its own
// app at its own origin. Rewriting that to the hosted app would send an
// operator who deliberately left the hosted service straight back to it.
func TestUnit_Relaypair_SelfHostedRelayKeepsItsOwnOrigin(t *testing.T) {
	for _, endpoint := range []string{
		"https://relay.example.internal",
		"https://relay.example.internal:8443",
		"http://10.0.0.7:9000",
	} {
		got, err := relaypair.AppOrigin(endpoint)
		if err != nil {
			t.Fatalf("AppOrigin(%q): %v", endpoint, err)
		}
		if got != endpoint {
			t.Fatalf("AppOrigin(%q) = %q, want it unchanged", endpoint, got)
		}
	}
}

// The app's routes hang off the root, so a configured path is not part of the
// origin a link is built from.
func TestUnit_Relaypair_PathIsNotPartOfTheOrigin(t *testing.T) {
	got, err := relaypair.AppOrigin("https://relay.example.internal/relay/v1")
	if err != nil {
		t.Fatalf("AppOrigin: %v", err)
	}
	if got != "https://relay.example.internal" {
		t.Fatalf("AppOrigin kept the path: %q", got)
	}
}

// A stored endpoint that is not a URL must be reported, not silently turned
// into a link that goes nowhere.
func TestUnit_Relaypair_UnusableEndpointIsAnError(t *testing.T) {
	for _, endpoint := range []string{"", "not a url", "relay.example.internal", "://missing-scheme"} {
		if _, err := relaypair.AppOrigin(endpoint); err == nil {
			t.Fatalf("AppOrigin(%q) = nil error, want a failure", endpoint)
		}
	}
}
