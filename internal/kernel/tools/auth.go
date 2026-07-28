package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/yalp/jsonpath"
)

// AuthError indicates a 401 or 403 status was returned.
type AuthError struct {
	StatusCode int
	Body       string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("API request failed with status %d: %s", e.StatusCode, e.Body)
}

func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *AuthError
	return errors.As(err, &authErr)
}

func PerformAuthFlow(ctx context.Context, tools *runtimetypes.RemoteTools, client *http.Client) (map[string]ParamArg, error) {
	flow := tools.AuthFlow
	if flow == nil {
		return nil, fmt.Errorf("no auth flow configured")
	}

	bodyStr := flow.LoginBody
	if bodyStr != "" {
		bodyStr = os.ExpandEnv(bodyStr)
	}

	var reqBody io.Reader
	if bodyStr != "" {
		reqBody = bytes.NewBufferString(bodyStr)
	}

	req, err := http.NewRequestWithContext(ctx, flow.LoginMethod, flow.LoginURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login failed with status %d: %s", resp.StatusCode, string(b))
	}

	var extracted string
	var isCookie bool

	if flow.ExtractCookie != "" {
		for _, c := range resp.Cookies() {
			if c.Name == flow.ExtractCookie {
				extracted = c.Value
				isCookie = true
				break
			}
		}
		if extracted == "" {
			return nil, fmt.Errorf("cookie %q not found in login response", flow.ExtractCookie)
		}
	} else if flow.ExtractJSONPath != "" {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read login response: %w", err)
		}
		var data interface{}
		if err := json.Unmarshal(b, &data); err != nil {
			return nil, fmt.Errorf("failed to parse login response as JSON: %w", err)
		}

		res, err := jsonpath.Read(data, flow.ExtractJSONPath)
		if err != nil {
			return nil, fmt.Errorf("failed to extract token via jsonpath %q: %w", flow.ExtractJSONPath, err)
		}
		if s, ok := res.(string); ok {
			extracted = s
		} else {
			extracted = fmt.Sprintf("%v", res)
		}
	} else {
		return nil, fmt.Errorf("auth flow must specify either extractCookie or extractJsonPath")
	}

	injects := make(map[string]ParamArg)
	if flow.InjectHeader != "" {
		val := extracted
		if flow.InjectFormat != "" {
			val = fmt.Sprintf(flow.InjectFormat, extracted)
		} else if isCookie {
			val = fmt.Sprintf("%s=%s", flow.ExtractCookie, extracted)
		}
		injects[flow.InjectHeader] = ParamArg{
			Name:  flow.InjectHeader,
			Value: val,
			In:    ArgLocationHeader,
		}
	}

	return injects, nil
}
