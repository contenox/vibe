// Package relaypair redeems a pairing key for this machine's relay
// credentials via one POST; what comes back is what [relaycreds] stores.
// Validity is the relay's decision: a refused key produces [ErrKeyRejected]
// wrapping the relay's own message.
package relaypair

import (
	"bytes"
	"context"
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
)

// EndpointEnv overrides the relay to pair with.
const EndpointEnv = "CONTENOX_RELAY_ENDPOINT"

// DefaultEndpoint is the hosted relay, shipped in the binary; a self-hosted
// relay overrides it and is verified against its own key.
const DefaultEndpoint = "https://relay.contenox.com"

// DefaultAppEndpoint is where a human signs in to the hosted service: the
// app's canonical hostname.
const DefaultAppEndpoint = "https://app.contenox.com"

// PinnedRelayPublicKey is the hosted relay's Ed25519 identity key, base64,
// overridable via -ldflags; [Redeem] refuses any other key from
// [DefaultEndpoint].
var PinnedRelayPublicKey = "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE="

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

// AppOrigin reduces a relay endpoint to the origin (scheme://host) a human
// opens in a browser: the hosted relay maps to [DefaultAppEndpoint], and a
// self-hosted relay serves its own app at its own origin. Any path the
// endpoint was configured with is dropped, because the app's routes hang off
// the root.
//
// It lives here rather than beside a caller because both the session surface
// and the CLI print app links, and two derivations of the same origin is one
// more place for them to disagree.
func AppOrigin(endpoint string) (string, error) {
	if endpoint == DefaultEndpoint {
		endpoint = DefaultAppEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("the stored relay endpoint %q is not a URL an app link can be built from", endpoint)
	}
	return u.Scheme + "://" + u.Host, nil
}

const httpTimeout = 30 * time.Second

// Failures a caller distinguishes.
var (
	// ErrNoEndpoint reports no relay configured: a setup fault, not a bad key.
	ErrNoEndpoint = errors.New("relaypair: no relay endpoint configured")
	// ErrKeyRejected reports the relay refused the key, wrapping its message.
	ErrKeyRejected = errors.New("relaypair: the relay refused the pairing key")
	// ErrRelayUnusable reports an answer missing a credential or carrying an
	// unusable identity key.
	ErrRelayUnusable = errors.New("relaypair: the relay's answer is unusable")
)

type redeemRequest struct {
	Key          string `json:"key"`
	InstanceName string `json:"instance_name"`
}

type redeemResponse struct {
	InstanceToken  string `json:"instance_token"`
	InstanceID     string `json:"instance_id"`
	AccountID      string `json:"account_id"`
	RelayPublicKey string `json:"relay_public_key"`
}

// Redeem exchanges key for this machine's credentials at endpoint; it does
// not store them, see [relaycreds.Save].
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
	// Parsed here, not on first dial, so an unusable key fails while a human is present.
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
