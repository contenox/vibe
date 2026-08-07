// Package relaypair redeems a pairing key for this machine's relay
// credentials: one POST, and what comes back is what [relaycreds] stores.
//
// The key is minted elsewhere and carried here by a human, so no browser is
// involved on this machine — a typed key behaves identically over SSH, in a
// container and under WSL.
//
// Validity is the relay's decision, never this package's. A refused key
// produces [ErrKeyRejected] wrapping the relay's own message; the relay does
// not distinguish unknown from expired from spent, so neither may this side.
package relaypair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/librelay"
)

// EndpointEnv overrides the relay to pair with.
const EndpointEnv = "CONTENOX_RELAY_ENDPOINT"

// DefaultEndpoint is the hosted relay, shipped in the binary.
//
// Shipping an address contacts nothing: [Redeem] is the only call that reaches
// a relay, and it runs only when a human types a pairing key. A self-hosted
// relay overrides it and is verified against its own key.
const DefaultEndpoint = "https://relay.contenox.com"

// PinnedRelayPublicKey is the hosted relay's Ed25519 identity key, base64.
//
// A var, not a const, so a build may override it:
//
//	-ldflags "-X github.com/contenox/contenox/internal/relaypair.PinnedRelayPublicKey=<base64>"
//
// Without it, pairing would trust whatever key the relay returned, and
// interception at pairing time could substitute an identity that every later
// handshake then verifies against correctly. [Redeem] refuses any other key
// from [DefaultEndpoint].
//
// It is the long-lived identity key, never the TLS leaf, which rotates roughly
// every 90 days. Only [DefaultEndpoint] is checked: a self-hosted relay has its
// own key, delivered at pairing.
var PinnedRelayPublicKey = "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE="

// init refuses a build that cannot recognise its own relay.
//
// A binary with no pin, or an unusable one, would pair against any identity
// presented to it — and would do so silently, on machines nobody can reach.
// Failing at process start makes that a build error rather than a field
// failure. It is deliberately unconditional: gating it on "only when pairing is
// used" is how a broken build reaches the one user who uses it.
func init() {
	if strings.TrimSpace(PinnedRelayPublicKey) == "" {
		panic("relaypair: built with no pinned relay public key; " +
			"set relaypair.PinnedRelayPublicKey at build time")
	}
	if _, err := librelay.ParsePublicKey(PinnedRelayPublicKey); err != nil {
		panic("relaypair: pinned relay public key is unusable: " + err.Error())
	}
}

// Endpoint resolves which relay to pair with: an explicit choice, then
// [EndpointEnv], then [DefaultEndpoint].
func Endpoint(explicit string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	if e := strings.TrimSpace(os.Getenv(EndpointEnv)); e != "" {
		return e
	}
	return DefaultEndpoint
}

// httpTimeout bounds one redemption.
const httpTimeout = 30 * time.Second

// Failures a caller distinguishes.
var (
	// ErrNoEndpoint reports no relay configured: a setup fault, not a bad key.
	ErrNoEndpoint = errors.New("relaypair: no relay endpoint configured")
	// ErrKeyRejected reports the relay refused the key, wrapping its message.
	// The relay answers unknown, expired and spent identically; this side must
	// not resolve them further.
	ErrKeyRejected = errors.New("relaypair: the relay refused the pairing key")
	// ErrRelayUnusable reports an answer missing a credential or carrying an
	// unusable identity key. Refused rather than stored: a stored bad key
	// fails every later dial instead of this one.
	ErrRelayUnusable = errors.New("relaypair: the relay's answer is unusable")
)

// redeemRequest is the body of POST /v1/pair/redeem.
type redeemRequest struct {
	Key          string `json:"key"`
	InstanceName string `json:"instance_name"`
}

// redeemResponse is the relay's answer: [relaycreds.Credentials] minus the
// endpoint, which the caller already holds.
type redeemResponse struct {
	InstanceToken  string `json:"instance_token"`
	InstanceID     string `json:"instance_id"`
	AccountID      string `json:"account_id"`
	RelayPublicKey string `json:"relay_public_key"`
}

// Redeem exchanges key for this machine's credentials at endpoint. It does not
// store them; see [relaycreds.Save].
func Redeem(ctx context.Context, client *http.Client, endpoint, key, instanceName string) (relaycreds.Credentials, error) {
	var zero relaycreds.Credentials
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return zero, ErrNoEndpoint
	}
	if strings.TrimSpace(key) == "" {
		return zero, fmt.Errorf("%w: no key given", ErrKeyRejected)
	}
	if strings.TrimSpace(instanceName) == "" {
		return zero, errors.New("relaypair: this machine has no name to pair under")
	}
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}

	body, err := json.Marshal(redeemRequest{Key: key, InstanceName: instanceName})
	if err != nil {
		return zero, fmt.Errorf("relaypair: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"/v1/pair/redeem", bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("relaypair: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("relaypair: reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	// Bounded: the configured address may be anything at all.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, fmt.Errorf("relaypair: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("%w: %s", ErrKeyRejected, relayMessage(raw, resp.StatusCode))
	}

	var out redeemResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrRelayUnusable, err)
	}
	if out.InstanceToken == "" || out.InstanceID == "" {
		return zero, fmt.Errorf("%w: it returned no instance credential", ErrRelayUnusable)
	}
	if out.RelayPublicKey == "" {
		return zero, fmt.Errorf("%w: it returned no public key, so this machine "+
			"would have no way to tell that relay from any other", ErrRelayUnusable)
	}
	// Parsed here, not on first dial: an unusable key is fatal to every later
	// connection, so it must fail while a human is present.
	if _, err := librelay.ParsePublicKey(out.RelayPublicKey); err != nil {
		return zero, fmt.Errorf("%w: its public key is unusable: %v", ErrRelayUnusable, err)
	}
	// Hosted endpoint only; see [PinnedRelayPublicKey].
	if endpoint == DefaultEndpoint && out.RelayPublicKey != PinnedRelayPublicKey {
		return zero, fmt.Errorf("%w: it presented a different identity than this "+
			"build expects, so it is not the relay it claims to be", ErrRelayUnusable)
	}

	return relaycreds.Credentials{
		Endpoint:       endpoint,
		InstanceToken:  out.InstanceToken,
		InstanceID:     out.InstanceID,
		AccountID:      out.AccountID,
		RelayPublicKey: out.RelayPublicKey,
	}, nil
}

// relayMessage extracts the relay's message from an error body, falling back to
// the status text. The configured address may serve any shape, so no field is
// assumed present.
func relayMessage(raw []byte, status int) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && len(trimmed) <= 200 {
		return trimmed
	}
	return http.StatusText(status)
}
