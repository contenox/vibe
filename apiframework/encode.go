// Package apiframework provides HTTP request/response helpers for the Contenox API.
// For OpenAPI generation (tools/openapi-gen), place // @request pkg.Type after Decode and // @response pkg.Type
// after Encode; path and query documentation uses the description arguments on GetPathParam / GetQueryParam.
// Keep those helper calls in the route handler body so the static generator can discover them directly.
package apiframework

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ErrEncodeInvalidJSON      = errors.New("serverops: encoding failing, invalid json")
	ErrDecodeInvalidJSON      = errors.New("serverops: decoding failing, invalid json")
	ErrDecodeInvalidYAML      = errors.New("serverops: decoding failing, invalid yaml")
	ErrDecodeBase64           = errors.New("serverops: decoding failing, invalid base64 data")
	ErrUnsupportedContentType = errors.New("serverops: unsupported content type for decoding")
	ErrReadingRequestBody     = errors.New("serverops: failed to read request body")
	ErrMalformedContentType   = errors.New("serverops: malformed Content-Type header")
)

func Encode[T any](w http.ResponseWriter, _ *http.Request, status int, v T) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrEncodeInvalidJSON, err)
	}
	return nil
}

func Decode[T any](r *http.Request) (T, error) {
	var v T

	contentTypeHeader := r.Header.Get("Content-Type")

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return v, fmt.Errorf("%w: %w", ErrReadingRequestBody, err)
	}
	defer r.Body.Close()

	var mediaType string
	var params map[string]string

	if contentTypeHeader == "" {
		mediaType = ""
	} else {
		var parseErr error
		mediaType, params, parseErr = mime.ParseMediaType(contentTypeHeader)
		if parseErr != nil {
			return v, fmt.Errorf("%w: %s - %v", ErrMalformedContentType, contentTypeHeader, parseErr)
		}
	}

	switch strings.ToLower(mediaType) {
	case "application/json":
		if err := json.Unmarshal(bodyBytes, &v); err != nil {
			return v, fmt.Errorf("%w: %w", ErrDecodeInvalidJSON, err)
		}
		return v, nil

	case "application/yaml", "text/yaml", "application/x-yaml":
		if enc, ok := params["encoding"]; ok && strings.ToLower(enc) == "base64" {
			decodedBytes, err := base64.StdEncoding.DecodeString(string(bodyBytes))
			if err != nil {
				return v, fmt.Errorf("%w: while decoding base64 for YAML: %w", ErrDecodeBase64, err)
			}
			if err := yaml.Unmarshal(decodedBytes, &v); err != nil {
				return v, fmt.Errorf("%w: after base64 decoding: %w", ErrDecodeInvalidYAML, err)
			}
			return v, nil
		}
		if err := yaml.Unmarshal(bodyBytes, &v); err != nil {
			return v, fmt.Errorf("%w: %w", ErrDecodeInvalidYAML, err)
		}
		return v, nil

	case "application/x-yaml-base64":
		decodedBytes, err := base64.StdEncoding.DecodeString(string(bodyBytes))
		if err != nil {
			return v, fmt.Errorf("%w: for %s: %w", ErrDecodeBase64, mediaType, err)
		}
		if err := yaml.Unmarshal(decodedBytes, &v); err != nil {
			return v, fmt.Errorf("%w: for %s after base64 decoding: %w", ErrDecodeInvalidYAML, mediaType, err)
		}
		return v, nil

	case "":
		if err := json.Unmarshal(bodyBytes, &v); err != nil {
			return v, fmt.Errorf("no Content-Type provided, and failed to decode as JSON: %w", err)
		}
		return v, nil

	default:
		return v, fmt.Errorf("%w: %s", ErrUnsupportedContentType, mediaType)
	}
}
