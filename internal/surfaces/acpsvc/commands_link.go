package acpsvc

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/contenox/contenox/internal/relaycreds"
	"github.com/contenox/contenox/internal/relaypair"
	libacp "github.com/contenox/contenox/libacp"
)

// handleLink prints the app deep link for the session it is typed into: the
// app's origin plus its attached route, /session/<instance id>/<session id>.
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

// appOrigin resolves where the app serving this session's link lives, given the
// relay endpoint a pairing stored.
func appOrigin(endpoint string) (string, error) {
	origin, err := relaypair.AppOrigin(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w — /pair again", err)
	}
	return origin, nil
}
