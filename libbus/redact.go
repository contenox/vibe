package libbus

import (
	"net/url"
	"strings"
)

const maskedCredential = "xxxxx"

// redactURL masks credentials in a NATS server URL, or a comma-separated
// list of them (as nats.Connect and Config.NATSURL accept), before it is
// logged: libbus logs through the standard "log" package rather than slog,
// so a caller that redirects the process's slog output does not stop these
// lines from reaching stderr.
func redactURL(raw string) string {
	parts := strings.Split(raw, ",")
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if masked := redactOne(trimmed); masked != trimmed {
			parts[i] = strings.Replace(part, trimmed, masked, 1)
		}
	}
	return strings.Join(parts, ",")
}

func redactOne(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return maskUserinfo(raw)
	}
	if _, set := u.User.Password(); set {
		u.User = url.UserPassword(u.User.Username(), maskedCredential)
	} else {
		u.User = url.User(maskedCredential)
	}
	return u.String()
}

// maskUserinfo finds '@' scanning from the end, since a password may itself contain '/', '?' or '#'.
func maskUserinfo(raw string) string {
	sep := strings.Index(raw, "://")
	if sep < 0 {
		return raw
	}
	rest := raw[sep+len("://"):]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	masked := maskedCredential
	if colon := strings.Index(rest[:at], ":"); colon >= 0 {
		masked = rest[:colon] + ":" + maskedCredential
	}
	return raw[:sep+len("://")] + masked + rest[at:]
}
