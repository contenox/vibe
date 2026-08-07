// login_cmd.go is the client half of RFC 8628 device authorisation: it asks a
// relay for a device code, prints what a human must type where, polls until the
// human has approved it, and stores the resulting enrolment.
//
// It never opens a browser. Half the fleet is headless boxes, containers and
// WSL, where there is no browser to open and an attempt to launch one is a
// failure mode rather than a convenience — printing the URL is the interface,
// and the human approves on whatever device they happen to be holding.
package contenoxcli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/librelay"
	"github.com/spf13/cobra"
)

// Device-authorisation defaults. They apply only when a relay omits the
// corresponding field; a relay that states an interval or a lifetime always
// wins, because it is the side that knows what its codes cost.
const (
	deviceDefaultInterval = 5 * time.Second
	deviceMinInterval     = 1 * time.Second
	deviceMaxInterval     = 60 * time.Second
	deviceSlowDownStep    = 5 * time.Second
	deviceDefaultLifetime = 10 * time.Minute
	deviceHTTPTimeout     = 30 * time.Second
)

// relayEndpointEnv names the relay to enrol with when --endpoint is absent.
// The endpoint is configuration and this build ships no default: a runtime
// with nothing configured talks to no relay at all, which is the only safe
// value for software that runs on other people's machines.
const relayEndpointEnv = "CONTENOX_RELAY_ENDPOINT"

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Enrol this machine with a relay.",
	Long: `Enrol this machine with a relay so it can be reached from elsewhere.

Prints a URL and a short code. Open the URL on any device you can sign in on —
a phone, a laptop, anything with a browser — enter the code, and this command
finishes on its own. Nothing is opened on this machine, so it works the same
over SSH, in a container and under WSL.

The relay endpoint is configuration: pass --endpoint, set ` + relayEndpointEnv + `,
or pass an enrollment token from a self-hosted relay with --enrollment-token.

The credentials land in ~/.contenox by default, because they identify this
machine rather than a project. Pass --data-dir to keep them somewhere else.`,
	Args: cobra.NoArgs,
	RunE: runLogin,
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Forget this machine's relay enrolment.",
	Long: `Delete the relay credentials stored on this machine.

This is local only: it stops this machine dialing the relay, and it does not
revoke anything. Revoking an instance is done at the relay, and a revoked
instance is refused the next time it dials whether or not it still has the
file.`,
	Args: cobra.NoArgs,
	RunE: runLogout,
}

func init() {
	loginCmd.Flags().String("endpoint", "", "Relay endpoint to enrol with (or set "+relayEndpointEnv+")")
	loginCmd.Flags().String("name", "", "Name to register this machine under (default: hostname)")
	loginCmd.Flags().String("enrollment-token", "", "Enrollment token from a self-hosted relay, carrying its endpoint and public key")
	rootCmd.AddCommand(loginCmd, logoutCmd)
}

// relayCredentialsDir is where this machine's relay enrolment is read and
// written.
//
// The default is the home directory, deliberately, and not the cwd-walk every
// other verb uses: a relay enrolment describes the *machine*, not the directory
// somebody happened to be standing in when they ran the command. Defaulting to
// the walk would enrol one machine twice from two project directories, and
// `contenox acp` and the terminal UI read the home directory only, so a
// project-local credential would be written and then never found.
//
// --data-dir overrides it, because "both" is the honest answer: a scoped
// credential is a legitimate thing to want, the flag already means exactly
// "use this .contenox directory", and inventing a second flag for the same
// idea would be the split this comment exists to avoid repeating. Every read
// goes through this function, so login and use cannot disagree about where the
// file is.
func relayCredentialsDir(cmd *cobra.Command) (string, error) {
	if cmd != nil {
		if dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir"); dataDir != "" {
			return ResolveContenoxDir(cmd)
		}
	}
	return globalContenoxDir()
}

// deviceCodeResponse is the relay's answer to the device-code request.
type deviceCodeResponse struct {
	// DeviceCode is the secret this machine polls with. It is never
	// displayed: the human types UserCode, and a device code on a screen is
	// a device code in a screenshot.
	DeviceCode string `json:"device_code"`
	// UserCode is short and human-typable, and is what gets printed.
	UserCode string `json:"user_code"`
	// VerificationURI is where the human enters UserCode.
	VerificationURI string `json:"verification_uri"`
	// Interval is the relay's requested polling period, in seconds.
	Interval int `json:"interval"`
	// ExpiresIn bounds the whole exchange, in seconds. Polling past it is
	// polling for an answer that can no longer arrive.
	ExpiresIn int `json:"expires_in"`
}

// deviceTokenResponse is a completed enrolment. The account, not the approving
// user, owns the instance.
type deviceTokenResponse struct {
	InstanceToken  string `json:"instance_token"`
	InstanceID     string `json:"instance_id"`
	AccountID      string `json:"account_id"`
	RelayPublicKey string `json:"relay_public_key"`
}

// deviceErrorResponse is RFC 8628's error body. The code is what is branched
// on; the description is for the operator.
type deviceErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// RFC 8628 error codes this client acts on. Anything else ends the exchange:
// an unrecognized error is not an invitation to keep polling.
const (
	deviceErrPending  = "authorization_pending"
	deviceErrSlowDown = "slow_down"
)

// errDeviceExpired ends a poll whose code outlived its approval window.
var errDeviceExpired = errors.New("the code expired before it was approved")

// enrollmentToken is what a self-hosted relay hands an operator instead of a
// hostname: the same two facts a hosted enrolment would have produced, so one
// binary serves both and no per-deployment build exists.
type enrollmentToken struct {
	Endpoint       string `json:"endpoint"`
	RelayPublicKey string `json:"relay_public_key"`
}

// deviceFlow runs the exchange. The clock and the HTTP client are fields so a
// test can drive a full slow_down escalation without waiting out real
// intervals; nothing else about the flow changes between a test and a machine.
type deviceFlow struct {
	client *http.Client
	// base is the relay's origin. Only scheme and host are used: the device
	// endpoints are fixed paths, and an endpoint configured with the
	// connector's path on it must not turn into /v1/connect/v1/device/code.
	base  *url.URL
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// newDeviceFlow builds a flow against endpoint.
func newDeviceFlow(endpoint string) (*deviceFlow, error) {
	u, err := parseRelayOrigin(endpoint)
	if err != nil {
		return nil, err
	}
	return &deviceFlow{
		client: &http.Client{Timeout: deviceHTTPTimeout},
		base:   u,
		now:    time.Now,
		sleep:  sleepCtx,
	}, nil
}

// parseRelayOrigin reads a configured endpoint down to scheme and host. A bare
// host is read as https and nothing else is accepted: the device code and the
// instance token both cross this connection, and a cleartext fallback that
// configuration could reach for is not a fallback, it is a way to lose them.
func parseRelayOrigin(endpoint string) (*url.URL, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, fmt.Errorf("no relay endpoint: pass --endpoint or set %s", relayEndpointEnv)
	}
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("relay endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("relay endpoint %q: must be https", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("relay endpoint %q: no host", endpoint)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// sleepCtx waits, or returns early when the command is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// post sends a JSON body and returns the status and the raw answer. It reads
// a bounded amount: an enrolment response is small, and a relay that answers a
// device-code request with a stream is not one to allocate for.
func (d *deviceFlow) post(ctx context.Context, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	u := *d.base
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// requestCode starts the exchange.
func (d *deviceFlow) requestCode(ctx context.Context, instanceName string) (deviceCodeResponse, error) {
	var out deviceCodeResponse
	status, raw, err := d.post(ctx, "/v1/device/code", map[string]string{"instance_name": instanceName})
	if err != nil {
		return out, fmt.Errorf("request device code: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return out, fmt.Errorf("request device code: %s", describeDeviceError(status, raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("request device code: parse answer: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return out, errors.New("request device code: the relay's answer is missing a code or a verification URI")
	}
	return out, nil
}

// poll waits for the human, at the interval the relay asked for.
//
// The interval only ever grows: slow_down means the relay is telling this
// client it is polling too fast, and a client that acknowledged that and then
// reverted on the next tick would be ignoring it. The deadline is the relay's
// own expires_in, so a code that can no longer be approved stops being polled
// for rather than being polled for forever.
func (d *deviceFlow) poll(ctx context.Context, code deviceCodeResponse) (deviceTokenResponse, error) {
	var out deviceTokenResponse
	interval := deviceDefaultInterval
	if code.Interval > 0 {
		interval = time.Duration(code.Interval) * time.Second
	}
	interval = clampInterval(interval)

	lifetime := deviceDefaultLifetime
	if code.ExpiresIn > 0 {
		lifetime = time.Duration(code.ExpiresIn) * time.Second
	}
	deadline := d.now().Add(lifetime)

	for {
		if err := d.sleep(ctx, interval); err != nil {
			return out, err
		}
		if d.now().After(deadline) {
			return out, errDeviceExpired
		}
		status, raw, err := d.post(ctx, "/v1/device/token", map[string]string{"device_code": code.DeviceCode})
		if err != nil {
			// A transient network failure mid-poll is not a refusal:
			// the code is still good until it expires, and giving up
			// here would make a dropped packet look like a denial.
			continue
		}
		var derr deviceErrorResponse
		_ = json.Unmarshal(raw, &derr)
		switch {
		case status == http.StatusOK:
			if err := json.Unmarshal(raw, &out); err != nil {
				return out, fmt.Errorf("parse enrolment: %w", err)
			}
			if out.InstanceToken == "" || out.InstanceID == "" {
				return deviceTokenResponse{}, errors.New("the relay approved the pairing but returned no instance token")
			}
			return out, nil
		case derr.Error == deviceErrSlowDown || status == http.StatusTooManyRequests:
			interval = clampInterval(interval + deviceSlowDownStep)
		case derr.Error == deviceErrPending ||
			status == http.StatusPreconditionRequired || status == http.StatusAccepted:
			// Still waiting on the human.
		default:
			return out, errors.New(describeDeviceError(status, raw))
		}
	}
}

// clampInterval keeps a polling period inside what is worth doing: below the
// floor is a hot loop against a relay, above the ceiling is a human staring at
// a finished approval.
func clampInterval(d time.Duration) time.Duration {
	if d < deviceMinInterval {
		return deviceMinInterval
	}
	if d > deviceMaxInterval {
		return deviceMaxInterval
	}
	return d
}

// describeDeviceError renders a relay's refusal for an operator, preferring the
// structured code over a status number.
func describeDeviceError(status int, raw []byte) string {
	var derr deviceErrorResponse
	if err := json.Unmarshal(raw, &derr); err == nil && derr.Error != "" {
		if derr.Description != "" {
			return fmt.Sprintf("the relay refused: %s (%s)", derr.Error, derr.Description)
		}
		return fmt.Sprintf("the relay refused: %s", derr.Error)
	}
	body := strings.TrimSpace(string(raw))
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("the relay answered %d", status)
	}
	return fmt.Sprintf("the relay answered %d: %s", status, body)
}

// decodeEnrollmentToken reads a self-hosted relay's token. It is base64 over
// the same JSON a hosted enrolment would have produced, so the verification
// path is identical and only the key differs.
func decodeEnrollmentToken(s string) (enrollmentToken, error) {
	var t enrollmentToken
	s = strings.TrimSpace(s)
	var raw []byte
	decoded := false
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		b, err := enc.DecodeString(s)
		if err == nil {
			raw, decoded = b, true
			break
		}
	}
	if !decoded {
		return t, errors.New("enrollment token is not base64")
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("enrollment token: %w", err)
	}
	if t.Endpoint == "" {
		return t, errors.New("enrollment token carries no endpoint")
	}
	return t, nil
}

// runLogin performs the enrolment and stores what it produced.
func runLogin(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()

	endpoint, _ := cmd.Flags().GetString("endpoint")
	tokenFlag, _ := cmd.Flags().GetString("enrollment-token")
	var enrol enrollmentToken
	if tokenFlag != "" {
		t, err := decodeEnrollmentToken(tokenFlag)
		if err != nil {
			return err
		}
		enrol = t
		if endpoint == "" {
			endpoint = t.Endpoint
		}
	}
	if endpoint == "" {
		endpoint = os.Getenv(relayEndpointEnv)
	}

	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			return errors.New("cannot determine this machine's hostname: pass --name")
		}
		name = host
	}

	dir, err := relayCredentialsDir(cmd)
	if err != nil {
		return err
	}

	flow, err := newDeviceFlow(endpoint)
	if err != nil {
		return err
	}
	code, err := flow.requestCode(ctx, name)
	if err != nil {
		return err
	}

	// Printed, never opened. Flushed before the first poll, because the
	// human cannot act on instructions that are still in a buffer.
	fmt.Fprintf(out, "\nTo authorise %q, open this on any device you can sign in on:\n\n    %s\n\n",
		name, code.VerificationURI)
	fmt.Fprintf(out, "and enter the code:\n\n    %s\n\n", code.UserCode)
	if code.ExpiresIn > 0 {
		fmt.Fprintf(out, "Waiting for approval (the code expires in %s)…\n",
			(time.Duration(code.ExpiresIn) * time.Second).Round(time.Minute))
	} else {
		fmt.Fprintln(out, "Waiting for approval…")
	}

	tok, err := flow.poll(ctx, code)
	if err != nil {
		return err
	}

	key := tok.RelayPublicKey
	if key == "" {
		// A self-hosted enrolment token carries the key out of band; a
		// hosted relay returns it. One of the two must have.
		key = enrol.RelayPublicKey
	}
	if key == "" {
		return errors.New("the relay returned no public key: this machine would have no way to tell that relay from any other, so the enrolment is refused")
	}
	if _, err := librelay.ParsePublicKey(key); err != nil {
		// Refused here rather than stored: a key that cannot be parsed
		// makes every future dial fail fatally, and the place to find
		// that out is while a human is still watching.
		return fmt.Errorf("the relay's public key is unusable: %w", err)
	}

	if err := relaycreds.Save(dir, relaycreds.Credentials{
		Endpoint:       endpoint,
		InstanceToken:  tok.InstanceToken,
		InstanceID:     tok.InstanceID,
		AccountID:      tok.AccountID,
		RelayPublicKey: key,
	}); err != nil {
		return err
	}

	fmt.Fprintf(out, "\nEnrolled as instance %s.\n", tok.InstanceID)
	if tok.AccountID != "" {
		fmt.Fprintf(out, "Attached to account %s.\n", tok.AccountID)
	}
	fmt.Fprintf(out, "Credentials written to %s\n", relaycreds.Path(dir))
	return nil
}

// runLogout deletes the local enrolment.
func runLogout(cmd *cobra.Command, _ []string) error {
	dir, err := relayCredentialsDir(cmd)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if _, err := relaycreds.Load(dir); err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			fmt.Fprintf(out, "No relay credentials at %s — nothing to do.\n", relaycreds.Path(dir))
			return nil
		}
		// A credential file that will not parse is still a credential
		// file: deleting it is the point of the command.
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
	if err := relaycreds.Delete(dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s\n", relaycreds.Path(dir))
	fmt.Fprintln(out, "This machine will no longer dial the relay. Revoking the instance is done at the relay.")
	return nil
}
