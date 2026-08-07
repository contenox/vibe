package contenoxcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/spf13/cobra"
)

// testFlow wires a deviceFlow to h with a clock a test drives, so a full
// slow_down escalation runs in no time at all and the intervals are asserted
// rather than waited out.
func testFlow(t *testing.T, h http.Handler) (*deviceFlow, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	slept := []time.Duration{}
	now := time.Unix(0, 0)
	f := &deviceFlow{
		client: srv.Client(),
		base:   &url.URL{Scheme: u.Scheme, Host: u.Host},
		now:    func() time.Time { return now },
		sleep: func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			now = now.Add(d)
			return ctx.Err()
		},
	}
	return f, &slept
}

// TestUnit_DeviceFlowPollsUntilApproved is the ordinary enrolment: the relay
// says "not yet" until a human has approved, and the client keeps its interval.
func TestUnit_DeviceFlowPollsUntilApproved(t *testing.T) {
	t.Parallel()
	polls := 0
	f, slept := testFlow(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["device_code"] != "dev-secret" {
			t.Errorf("polled with device_code %q", body["device_code"])
		}
		polls++
		if polls < 3 {
			w.WriteHeader(http.StatusPreconditionRequired)
			_ = json.NewEncoder(w).Encode(deviceErrorResponse{Error: deviceErrPending})
			return
		}
		_ = json.NewEncoder(w).Encode(deviceTokenResponse{
			InstanceToken: "tok", InstanceID: "inst-a", AccountID: "acct-1", RelayPublicKey: "key",
		})
	}))

	got, err := f.poll(context.Background(), deviceCodeResponse{
		DeviceCode: "dev-secret", UserCode: "ABCD-EFGH", Interval: 2, ExpiresIn: 600,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.InstanceID != "inst-a" || got.InstanceToken != "tok" {
		t.Fatalf("poll = %+v", got)
	}
	for _, d := range *slept {
		if d != 2*time.Second {
			t.Fatalf("polling intervals %v, want a steady 2s", *slept)
		}
	}
}

// TestUnit_DeviceFlowHonoursSlowDown is RFC 8628's back-pressure: the relay
// says it is being polled too fast, and the interval only ever grows. A client
// that acknowledged slow_down and reverted on the next tick would be ignoring
// it.
func TestUnit_DeviceFlowHonoursSlowDown(t *testing.T) {
	t.Parallel()
	polls := 0
	f, slept := testFlow(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		switch polls {
		case 1, 2:
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(deviceErrorResponse{Error: deviceErrSlowDown})
		case 3:
			w.WriteHeader(http.StatusPreconditionRequired)
			_ = json.NewEncoder(w).Encode(deviceErrorResponse{Error: deviceErrPending})
		default:
			_ = json.NewEncoder(w).Encode(deviceTokenResponse{InstanceToken: "tok", InstanceID: "inst-a"})
		}
	}))

	if _, err := f.poll(context.Background(), deviceCodeResponse{
		DeviceCode: "dev", Interval: 5, ExpiresIn: 6000,
	}); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Each wait precedes the poll it throttles, so the escalation shows up on
	// the wait after the refusal, and the interval never falls back once the
	// relay stops complaining.
	want := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 15 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("intervals %v, want %v", *slept, want)
	}
	for i, d := range want {
		if (*slept)[i] != d {
			t.Fatalf("intervals %v, want %v", *slept, want)
		}
	}
}

// TestUnit_DeviceFlowStopsAtExpiry: a code that can no longer be approved is
// not polled for forever.
func TestUnit_DeviceFlowStopsAtExpiry(t *testing.T) {
	t.Parallel()
	f, _ := testFlow(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionRequired)
		_ = json.NewEncoder(w).Encode(deviceErrorResponse{Error: deviceErrPending})
	}))
	_, err := f.poll(context.Background(), deviceCodeResponse{DeviceCode: "dev", Interval: 5, ExpiresIn: 20})
	if !errors.Is(err, errDeviceExpired) {
		t.Fatalf("poll = %v, want errDeviceExpired", err)
	}
}

// TestUnit_DeviceFlowStopsOnARefusal keeps an unrecognized error from being
// read as an invitation to keep polling.
func TestUnit_DeviceFlowStopsOnARefusal(t *testing.T) {
	t.Parallel()
	polls := 0
	f, _ := testFlow(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(deviceErrorResponse{Error: "access_denied", Description: "the human said no"})
	}))
	_, err := f.poll(context.Background(), deviceCodeResponse{DeviceCode: "dev", Interval: 1, ExpiresIn: 600})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("poll = %v, want the relay's refusal", err)
	}
	if polls != 1 {
		t.Fatalf("polled %d times after a refusal, want 1", polls)
	}
}

// TestUnit_DeviceCodeRequestReportsAMissingURI: without a verification URI
// there is nothing to show a human, so the enrolment cannot proceed.
func TestUnit_DeviceCodeRequestReportsAMissingURI(t *testing.T) {
	t.Parallel()
	f, _ := testFlow(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/device/code" {
			t.Errorf("device code requested at %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(deviceCodeResponse{DeviceCode: "d", UserCode: "u"})
	}))
	if _, err := f.requestCode(context.Background(), "box"); err == nil {
		t.Fatal("an answer with no verification URI was accepted")
	}
}

// TestUnit_RelayEndpointMustBeHTTPS keeps the device code and the instance
// token off a cleartext connection; configuration must not be able to make
// that mistake.
func TestUnit_RelayEndpointMustBeHTTPS(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "   ", "http://relay.invalid", "https://"} {
		if _, err := parseRelayOrigin(bad); err == nil {
			t.Errorf("parseRelayOrigin(%q) was accepted", bad)
		}
	}
	u, err := parseRelayOrigin("relay.invalid/v1/connect")
	if err != nil {
		t.Fatalf("parseRelayOrigin: %v", err)
	}
	// Only scheme and host survive: the device endpoints are absolute paths
	// and must not be appended to the connector's.
	if u.String() != "https://relay.invalid" {
		t.Fatalf("origin = %q, want https://relay.invalid", u.String())
	}
}

// TestUnit_EnrollmentTokenCarriesEndpointAndKey is the self-hosting path: one
// binary for everyone, the deployment's identity supplied as configuration.
func TestUnit_EnrollmentTokenCarriesEndpointAndKey(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(enrollmentToken{Endpoint: "https://relay.internal", RelayPublicKey: "abc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeEnrollmentToken(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("decodeEnrollmentToken: %v", err)
	}
	if got.Endpoint != "https://relay.internal" || got.RelayPublicKey != "abc" {
		t.Fatalf("token = %+v", got)
	}
	if _, err := decodeEnrollmentToken("not base64 at all!!"); err == nil {
		t.Fatal("a token that is not base64 was accepted")
	}
	if _, err := decodeEnrollmentToken(base64.StdEncoding.EncodeToString([]byte(`{"relay_public_key":"abc"}`))); err == nil {
		t.Fatal("a token with no endpoint was accepted")
	}
}

// TestUnit_CredentialsLiveInHomeUnlessDataDirSaysOtherwise pins the storage
// decision: machine-global by default, because the credential describes the
// machine, and --data-dir when somebody wants it scoped.
func TestUnit_CredentialsLiveInHomeUnlessDataDirSaysOtherwise(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	root := &cobra.Command{Use: "contenox"}
	root.PersistentFlags().String("data-dir", "", "")
	sub := &cobra.Command{Use: "login"}
	root.AddCommand(sub)

	got, err := relayCredentialsDir(sub)
	if err != nil {
		t.Fatalf("relayCredentialsDir: %v", err)
	}
	if want := filepath.Join(home, ".contenox"); got != want {
		t.Fatalf("default credential directory = %q, want %q", got, want)
	}

	scoped := filepath.Join(t.TempDir(), "project", ".contenox")
	if err := root.PersistentFlags().Set("data-dir", scoped); err != nil {
		t.Fatalf("set --data-dir: %v", err)
	}
	got, err = relayCredentialsDir(sub)
	if err != nil {
		t.Fatalf("relayCredentialsDir with --data-dir: %v", err)
	}
	if got != scoped {
		t.Fatalf("credential directory = %q, want %q", got, scoped)
	}
	// Read and write must agree, which is the failure this function exists
	// to prevent: a credential written to one directory and looked for in
	// another is indistinguishable from never having logged in.
	if err := relaycreds.Save(got, relaycreds.Credentials{InstanceID: "inst-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := relayCredentialsDir(sub)
	if err != nil {
		t.Fatalf("relayCredentialsDir: %v", err)
	}
	if _, err := relaycreds.Load(back); err != nil {
		t.Fatalf("Load from %q: %v", back, err)
	}
}
