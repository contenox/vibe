package acpsvc

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
	libacp "github.com/contenox/contenox/libacp"
)

// handleLink prints the app deep link for the session it is typed into, so a
// person walking away from the desk can open this same session on a phone.
// One URL and one plain line, deliberately: phone-notification and clipboard
// ecosystems already move URLs between devices, so no QR, no clipboard magic.
//
// The link is the app's origin (see appOrigin) plus the app's attached route,
// /session/<instance id>/<session id> — the shape the app's router owns
// (its ROUTE_PATTERNS.attached), so a drift there breaks this link.
//
// sid is the ACP session id: the durable message-index name session/list
// reports, which is exactly the id the app's attached route addresses.
func (t *Transport) handleLink(sid libacp.SessionID) (string, error) {
	dir := t.deps.ContenoxDir
	if dir == "" {
		return "", fmt.Errorf("this session has no .contenox directory, so there is no pairing to link through")
	}
	creds, err := relaycreds.Load(dir)
	if err != nil {
		if errors.Is(err, relaycreds.ErrNotEnrolled) {
			return "This machine is not paired with a relay, so this session has no app link.\n" +
				"Sign in on the contenox app, tap Pair device, then run /pair <key> here.", nil
		}
		return "", fmt.Errorf("this machine's stored pairing cannot be read: %v — /unpair clears it", err)
	}
	origin, err := appOrigin(creds.Endpoint)
	if err != nil {
		return "", err
	}
	link := fmt.Sprintf("%s/session/%s/%s", origin,
		url.PathEscape(creds.InstanceID), url.PathEscape(string(sid)))
	return link + "\nOpen this on your phone to attach to this session — sign-in required.", nil
}

// appOrigin resolves where the app serving this session's link lives, given
// the relay endpoint a pairing stored.
//
// INVARIANT: the hosted relay and the hosted app are one deployment under two
// hostnames — machines dial [relaypair.DefaultEndpoint], humans sign in at
// [relaypair.DefaultAppEndpoint], the canonical human-facing address — so the
// hosted machine endpoint maps to the app's hostname. Any other endpoint is
// self-hosted, and a self-hosted relay serves the app same-origin, so its own
// origin is the app's.
func appOrigin(endpoint string) (string, error) {
	if endpoint == relaypair.DefaultEndpoint {
		endpoint = relaypair.DefaultAppEndpoint
	}
	return relayOrigin(endpoint)
}

// relayOrigin reduces an endpoint to its origin (scheme://host). The app's
// session routes hang off the root, so any path the endpoint was configured
// with does not belong in the link.
func relayOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("the stored relay endpoint %q is not a URL this session can be linked through — /pair again", endpoint)
	}
	return u.Scheme + "://" + u.Host, nil
}
