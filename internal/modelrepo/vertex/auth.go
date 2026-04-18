package vertex

import (
	"context"
	"fmt"

	"golang.org/x/oauth2/google"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

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
