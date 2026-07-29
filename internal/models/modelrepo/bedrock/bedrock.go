// Package bedrock is a provider for AWS Bedrock via the unified Converse API.
// Credentials are optional: a stored JSON blob of static AWS keys, or the
// ambient aws-sdk default chain (env / profile / IAM role / IMDS) when empty.
// The region is parsed from the backend `--url`. Unlike the other providers,
// the wire format is the AWS SDK's typed API, not hand-rolled JSON; the
// Bedrock-specific "codec" (converse.go) maps neutral types to SDK types.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/contenox/libtracker"
	"github.com/contenox/contenox/internal/models/modelrepo"
)

// classifyBedrockError maps the SDK's typed errors onto the modelrepo
// sentinels: ThrottlingException → ErrRateLimited; ValidationException whose
// message matches the documented "input is too long" phrasing →
// ErrContextLengthExceeded. Anything else passes through unchanged.
func classifyBedrockError(err error) error {
	if err == nil {
		return nil
	}
	var throttled *types.ThrottlingException
	if errors.As(err, &throttled) {
		return fmt.Errorf("%w: %w", modelrepo.ErrRateLimited, err)
	}
	var validation *types.ValidationException
	if errors.As(err, &validation) && modelrepo.IsContextLimitMessage(validation.ErrorMessage()) {
		return fmt.Errorf("%w: %w", modelrepo.ErrContextLengthExceeded, err)
	}
	return err
}

// bedrockBaseModelID strips a cross-region inference-profile geo prefix
// (us. / eu. / apac. / jp. / global.) so capability checks see the vendor
// model id regardless of how the model is invoked.
func bedrockBaseModelID(modelID string) string {
	for _, prefix := range []string{"us.", "eu.", "apac.", "jp.", "global."} {
		if strings.HasPrefix(modelID, prefix) {
			return strings.TrimPrefix(modelID, prefix)
		}
	}
	return modelID
}

// bedrockClient is the shared transport: a constructed SDK runtime client plus
// the model id and tracker.
type bedrockClient struct {
	api             *bedrockruntime.Client
	modelName       string
	maxOutputTokens int
	tracker         libtracker.ActivityTracker
}

// staticCreds is the optional stored credential blob (mirrors vertex's stored
// service-account JSON). Empty/absent → the ambient AWS default chain.
type staticCreds struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

// loadAWSConfig builds an aws.Config for the region, using the stored static
// credentials when present and otherwise the default credential chain.
func loadAWSConfig(ctx context.Context, region, credBlob string, httpClient *http.Client) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if httpClient != nil {
		opts = append(opts, awsconfig.WithHTTPClient(httpClient))
	}
	if strings.TrimSpace(credBlob) != "" {
		var c staticCreds
		if err := json.Unmarshal([]byte(credBlob), &c); err != nil {
			return aws.Config{}, fmt.Errorf("bedrock: parse stored credentials JSON: %w", err)
		}
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

// regionFromURL parses the AWS region from a Bedrock runtime URL of the form
// https://bedrock-runtime.<region>.amazonaws.com, or accepts a bare region
// string.
func regionFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not a URL — treat as a bare region (e.g. "us-east-1").
		if !strings.Contains(raw, "/") && !strings.Contains(raw, ".") {
			return raw
		}
		return ""
	}
	host := u.Host // bedrock-runtime.<region>.amazonaws.com
	parts := strings.Split(host, ".")
	for i, p := range parts {
		if (p == "bedrock-runtime" || p == "bedrock") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func chatConfigFromArgs(args []modelrepo.ChatArgument) *modelrepo.ChatConfig {
	cfg := &modelrepo.ChatConfig{}
	for _, a := range args {
		a.Apply(cfg)
	}
	return cfg
}

// documentToJSONString renders a smithy document (tool_use input) to a JSON
// string, matching the neutral ToolCall.Arguments contract.
func documentToJSONString(doc document.Interface) string {
	if doc == nil {
		return "{}"
	}
	// MarshalSmithyDocument yields the JSON representation directly and round-trips
	// for both wire-decoded documents and locally-built lazy documents.
	b, err := doc.MarshalSmithyDocument()
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// jsonStringToDocument parses a neutral ToolCall.Arguments JSON string into a
// smithy document for a Converse tool_use block.
func jsonStringToDocument(args string) document.Interface {
	var v any
	if strings.TrimSpace(args) == "" {
		v = map[string]any{}
	} else if err := json.Unmarshal([]byte(args), &v); err != nil {
		v = map[string]any{}
	}
	return document.NewLazyDocument(v)
}
