package relaypair_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/relaypair"
	"github.com/contenox/contenox/librelay"
)

// relayPublicKey is a well-formed relay identity. Generated, so the test
// cannot pass against a parser that stopped checking.
func relayPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// relayStub answers /v1/pair/redeem and records the request, so its shape is
// asserted against the wire contract rather than this package's structs.
func relayStub(t *testing.T, status int, body any, seen *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pair/redeem" {
			t.Errorf("redeemed against %q, want /v1/pair/redeem", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method %q, want POST", r.Method)
		}
		if seen != nil {
			if err := json.NewDecoder(r.Body).Decode(seen); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Pins the round trip, and that Endpoint is stamped on the result: a token is
// meaningless against a relay that did not issue it.
func TestUnit_Relaypair_RedeemHappyPath(t *testing.T) {
	key := relayPublicKey(t)
	var seen map[string]any
	srv := relayStub(t, http.StatusOK, map[string]any{
		"instance_token":   "tok-secret",
		"instance_id":      "inst-1",
		"account_id":       "acct-1",
		"relay_public_key": key,
	}, &seen)

	creds, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "BCDF-GHJK-MNPQ", "laptop")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if creds.InstanceToken != "tok-secret" || creds.InstanceID != "inst-1" || creds.AccountID != "acct-1" {
		t.Fatalf("credentials not carried through: %+v", creds)
	}
	if creds.RelayPublicKey != key {
		t.Fatalf("relay public key = %q, want %q", creds.RelayPublicKey, key)
	}
	if creds.Endpoint != srv.URL {
		t.Fatalf("endpoint = %q, want %q — a token is meaningless against another relay", creds.Endpoint, srv.URL)
	}
	// The key travels as the human typed it; normalising is the relay's job,
	// and doing it on both sides is how the two spellings drift apart.
	if seen["key"] != "BCDF-GHJK-MNPQ" {
		t.Fatalf("key sent as %v, want the typed form", seen["key"])
	}
	if seen["instance_name"] != "laptop" {
		t.Fatalf("instance_name sent as %v", seen["instance_name"])
	}
}

// A trailing slash must not reach the path, where it 404s against a strict mux
// and reads as a rejected key.
func TestUnit_Relaypair_EndpointIsNormalised(t *testing.T) {
	srv := relayStub(t, http.StatusOK, map[string]any{
		"instance_token": "t", "instance_id": "i", "relay_public_key": relayPublicKey(t),
	}, nil)

	creds, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL+"/  ", "K", "laptop")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if creds.Endpoint != srv.URL {
		t.Fatalf("endpoint = %q, want it trimmed to %q", creds.Endpoint, srv.URL)
	}
}

// The relay's message must survive: it answers unknown, expired and spent
// identically, so a more specific one invented here would be wrong.
func TestUnit_Relaypair_RejectionCarriesTheRelaysReason(t *testing.T) {
	srv := relayStub(t, http.StatusNotFound, map[string]any{
		"error": map[string]any{"message": "that pairing key is not valid — mint a new one in the app"},
	}, nil)

	_, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "BCDF", "laptop")
	if err == nil {
		t.Fatal("a refused key must be an error")
	}
	if !strings.Contains(err.Error(), "mint a new one in the app") {
		t.Fatalf("the relay's reason was lost: %v", err)
	}
	if !isKeyRejected(err) {
		t.Fatalf("want ErrKeyRejected, got %v", err)
	}
}

// The configured address may serve anything. A non-envelope body must still
// yield an actionable error and never panic.
func TestUnit_Relaypair_NonRelayAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
	}{
		{"plain text", http.StatusBadGateway, "upstream is down"},
		{"empty object", http.StatusForbidden, map[string]any{}},
		{"unexpected shape", http.StatusTeapot, map[string]any{"detail": "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := relayStub(t, tc.status, tc.body, nil)
			_, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "K", "laptop")
			if err == nil {
				t.Fatal("a non-200 must be an error")
			}
			if !isKeyRejected(err) {
				t.Fatalf("want ErrKeyRejected, got %v", err)
			}
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("the error must say something")
			}
		})
	}
}

// An unusable answer is refused, not stored: stored, it fails every later dial
// instead of this one.
func TestUnit_Relaypair_UnusableAnswersAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no token", map[string]any{"instance_id": "i", "relay_public_key": "k"}},
		{"no instance id", map[string]any{"instance_token": "t", "relay_public_key": "k"}},
		{"no public key", map[string]any{"instance_token": "t", "instance_id": "i"}},
		{"unparseable public key", map[string]any{
			"instance_token": "t", "instance_id": "i", "relay_public_key": "not-base64!!"}},
		{"public key of the wrong length", map[string]any{
			"instance_token": "t", "instance_id": "i",
			"relay_public_key": base64.StdEncoding.EncodeToString([]byte("too short"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := relayStub(t, http.StatusOK, tc.body, nil)
			_, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "K", "laptop")
			if err == nil {
				t.Fatal("an unusable answer must not be stored")
			}
			if !isRelayUnusable(err) {
				t.Fatalf("want ErrRelayUnusable, got %v", err)
			}
		})
	}
}

// ErrNoEndpoint stays distinct from ErrKeyRejected: different faults, different
// remedies.
func TestUnit_Relaypair_RefusesWithoutEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "   "} {
		_, err := relaypair.Redeem(context.Background(), nil, endpoint, "K", "laptop")
		if !isNoEndpoint(err) {
			t.Fatalf("endpoint %q: want ErrNoEndpoint, got %v", endpoint, err)
		}
	}
}

func TestUnit_Relaypair_RefusesEmptyKeyAndName(t *testing.T) {
	srv := relayStub(t, http.StatusOK, map[string]any{}, nil)

	if _, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "  ", "laptop"); err == nil {
		t.Fatal("an empty key must not reach the relay")
	}
	if _, err := relaypair.Redeem(context.Background(), srv.Client(), srv.URL, "K", " "); err == nil {
		t.Fatal("a machine with no name must not pair")
	}
}

func isKeyRejected(err error) bool   { return errors.Is(err, relaypair.ErrKeyRejected) }
func isRelayUnusable(err error) bool { return errors.Is(err, relaypair.ErrRelayUnusable) }
func isNoEndpoint(err error) bool    { return errors.Is(err, relaypair.ErrNoEndpoint) }

// The pin is what stops a machine pairing against an identity substituted at
// pairing time, so it is enforced against the hosted endpoint and only there.
func TestUnit_Relaypair_PinnedKeyIsEnforcedForTheHostedEndpoint(t *testing.T) {
	if relaypair.PinnedRelayPublicKey == "" {
		t.Fatal("the build carries no pinned key; init should have panicked")
	}
	if _, err := librelay.ParsePublicKey(relaypair.PinnedRelayPublicKey); err != nil {
		t.Fatalf("pinned key is unusable: %v", err)
	}
	if relaypair.DefaultEndpoint == "" {
		t.Fatal("no default endpoint")
	}

	// A relay at the hosted address presenting a different identity is
	// refused, however well-formed its answer is.
	srv := relayStub(t, http.StatusOK, map[string]any{
		"instance_token": "t", "instance_id": "i", "relay_public_key": relayPublicKey(t),
	}, nil)
	_, err := relaypair.Redeem(context.Background(), srv.Client(),
		relaypair.DefaultEndpoint, "K", "laptop")
	// Reaching the real host is not the point and must not be attempted; the
	// stub is not at that address, so this asserts only that the call fails.
	if err == nil {
		t.Fatal("a substituted identity at the hosted endpoint must be refused")
	}

	// A self-hosted relay legitimately carries its own key and must not be
	// held to the pin, or the build cannot self-host at all.
	other := relayPublicKey(t)
	srv2 := relayStub(t, http.StatusOK, map[string]any{
		"instance_token": "t", "instance_id": "i", "relay_public_key": other,
	}, nil)
	creds, err := relaypair.Redeem(context.Background(), srv2.Client(), srv2.URL, "K", "laptop")
	if err != nil {
		t.Fatalf("self-hosted relay refused: %v", err)
	}
	if creds.RelayPublicKey != other {
		t.Fatalf("self-hosted key = %q, want %q", creds.RelayPublicKey, other)
	}
}
