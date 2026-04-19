package mcpserverapi

import (
	"net/url"
	"strings"

	"github.com/contenox/contenox/runtime/mcpserverservice"
)

func oauthRedirectURL(res *mcpserverservice.OAuthCallbackResult, status, message string) string {
	base := ""
	name := ""
	if res != nil {
		base = strings.TrimRight(strings.TrimSpace(res.RedirectBase), "/")
		name = res.ServerName
	}

	target := "/hooks/remote"
	if base != "" {
		target = base + "/hooks/remote"
	}
	q := url.Values{}
	q.Set("mcp_oauth", status)
	if name != "" {
		q.Set("name", name)
	}
	if message != "" {
		q.Set("message", message)
	}
	return target + "?" + q.Encode()
}
