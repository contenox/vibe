package vertex

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewTokenSource returns a caching oauth2.TokenSource. Call this ONCE per
// provider and reuse the returned source — it caches the access token until
// expiry and only round-trips to the token endpoint on refresh. Creating a new
// source per request (as the BearerToken* helpers do) round-trips every time
// and adds ~100–400ms of auth latency to every chat/stream call.
//
// credJSON is the service account key JSON; empty falls back to ADC.
func NewTokenSource(ctx context.Context, credJSON string) (oauth2.TokenSource, error) {
	if credJSON != "" {
		creds, err := google.CredentialsFromJSON(ctx, []byte(credJSON), cloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("vertex AI service account credentials: %w", err)
		}
		return creds.TokenSource, nil
	}
	ts, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("vertex AI ADC: %w", err)
	}
	return ts, nil
}

// extractProjectFromVertexURL parses the GCP project ID from a Vertex AI base URL of the form
// https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}.
func extractProjectFromVertexURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// BearerToken returns a fresh ADC access token for Vertex AI.
func BearerToken(ctx context.Context) (string, error) {
	return BearerTokenWithCreds(ctx, "")
}

// BearerTokenWithCreds returns an access token using the provided service account
// JSON when non-empty, or ADC when empty.
func BearerTokenWithCreds(ctx context.Context, credJSON string) (string, error) {
	if credJSON != "" {
		creds, err := google.CredentialsFromJSON(ctx, []byte(credJSON), cloudPlatformScope)
		if err != nil {
			return "", fmt.Errorf("vertex AI service account credentials: %w", err)
		}
		tok, err := creds.TokenSource.Token()
		if err != nil {
			return "", fmt.Errorf("vertex AI service account token: %w", err)
		}
		return tok.AccessToken, nil
	}
	ts, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return "", fmt.Errorf("vertex AI ADC: %w", err)
	}
	tok, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("vertex AI token refresh: %w", err)
	}
	return tok.AccessToken, nil
}
