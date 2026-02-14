package jseval

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/contenox/vibe/libtracker"
	"github.com/dop251/goja"
)

// HTTPClient is the minimal interface we need from an HTTP client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// defaultHTTPClient is used if no custom client is provided.
var defaultHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
}

// httpFetchOptions is the shape we accept from JS when the first argument is an object.
type httpFetchOptions struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	TimeoutMs int               `json:"timeoutMs"`
}

// Simple typed errors so callers can distinguish mis-use vs network errors.
var (
	ErrMissingHTTPFetchArgument = errors.New("httpFetch requires at least one argument")
	ErrMissingHTTPFetchURL      = errors.New("httpFetch requires a non-empty 'url'")
)

// setupHTTPFetch installs a synchronous global `httpFetch` function into the VM.
//
// JS usage:
//
//	// Simple
//	const res = httpFetch("https://example.com");
//
//	// Advanced
//	const res = httpFetch({
//	  url: "https://example.com",
//	  method: "POST",
//	  headers: { "Content-Type": "application/json" },
//	  body: JSON.stringify({ hello: "world" }),
//	  timeoutMs: 10000,
//	});
//
// Returned object:
//
//	{
//	  ok: boolean,          // true if 2xx
//	  status: number,       // HTTP status code
//	  statusText: string,   // e.g. "200 OK"
//	  url: string,          // final URL after redirects
//	  headers: { ... },     // response headers (string->string)
//	  body: string,         // response body as UTF-8 string
//	  error: string|null    // network / internal error description
//	}
func setupHTTPFetch(
	vm *goja.Runtime,
	ctx context.Context,
	tracker libtracker.ActivityTracker,
	col *Collector,
	client HTTPClient,
) error {
	if vm == nil {
		return nil
	}
	if tracker == nil {
		tracker = libtracker.NoopTracker{}
	}
	if client == nil {
		client = defaultHTTPClient
	}

	return vm.Set("httpFetch", func(call goja.FunctionCall) goja.Value {
		var opts httpFetchOptions

		if len(call.Arguments) == 0 {
			extra := map[string]any{"error": "missing argument"}
			return withErrorReporting(vm, ctx, tracker, "http", "fetch", extra, func() (interface{}, error) {
				return nil, ErrMissingHTTPFetchArgument
			})
		}

		first := call.Arguments[0]
		exported := first.Export()

		if exported == nil {
			extra := map[string]any{"error": "argument is null/undefined"}
			return withErrorReporting(vm, ctx, tracker, "http", "fetch", extra, func() (interface{}, error) {
				return nil, ErrMissingHTTPFetchArgument
			})
		}

		if s, ok := exported.(string); ok {
			// httpFetch("https://...")
			opts.URL = strings.TrimSpace(s)
		} else {
			// httpFetch({ url, method, headers, body, timeoutMs })
			var parsed httpFetchOptions
			if err := vm.ExportTo(first, &parsed); err != nil {
				extra := map[string]any{"error": err.Error()}
				return withErrorReporting(vm, ctx, tracker, "http", "fetch", extra, func() (interface{}, error) {
					return nil, err
				})
			}
			opts = parsed
		}

		if opts.URL == "" {
			extra := map[string]any{"error": "url is empty"}
			return withErrorReporting(vm, ctx, tracker, "http", "fetch", extra, func() (interface{}, error) {
				return nil, ErrMissingHTTPFetchURL
			})
		}

		if opts.Method == "" {
			opts.Method = http.MethodGet
		}

		extra := map[string]any{
			"url":     opts.URL,
			"method":  opts.Method,
			"timeout": opts.TimeoutMs,
		}

		// Collector: record the call
		if col != nil {
			col.Add(ExecLogEntry{
				Timestamp: time.Now().UTC(),
				Kind:      "httpFetch",
				Name:      "httpFetch",
				Args:      []any{opts.URL, opts.Method},
				Meta: map[string]any{
					"url":     opts.URL,
					"method":  opts.Method,
					"phase":   "request",
					"timeout": opts.TimeoutMs,
				},
			})
		}

		return withErrorReporting(vm, ctx, tracker, "http", "fetch", extra, func() (interface{}, error) {
			// Optional per-call timeout override
			reqCtx := ctx
			var cancel context.CancelFunc
			if opts.TimeoutMs > 0 {
				reqCtx, cancel = context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
				defer cancel()
			}

			req, err := http.NewRequestWithContext(reqCtx, opts.Method, opts.URL, strings.NewReader(opts.Body))
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "httpFetch",
						Name:      "httpFetch",
						Error:     err.Error(),
						Meta: map[string]any{
							"url":    opts.URL,
							"method": opts.Method,
							"phase":  "build_request",
						},
					})
				}
				return nil, err
			}

			for k, v := range opts.Headers {
				req.Header.Set(k, v)
			}

			resp, err := client.Do(req)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "httpFetch",
						Name:      "httpFetch",
						Error:     err.Error(),
						Meta: map[string]any{
							"url":    opts.URL,
							"method": opts.Method,
							"phase":  "do_request",
						},
					})
				}
				return nil, err
			}
			defer resp.Body.Close()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				if col != nil {
					col.Add(ExecLogEntry{
						Timestamp: time.Now().UTC(),
						Kind:      "httpFetch",
						Name:      "httpFetch",
						Error:     err.Error(),
						Meta: map[string]any{
							"url":    opts.URL,
							"method": opts.Method,
							"phase":  "read_body",
						},
					})
				}
				return nil, err
			}

			// Flatten headers into string map
			headerMap := make(map[string]string, len(resp.Header))
			for k, vals := range resp.Header {
				headerMap[k] = strings.Join(vals, ", ")
			}

			ok := resp.StatusCode >= 200 && resp.StatusCode < 300

			result := map[string]any{
				"ok":         ok,
				"status":     resp.StatusCode,
				"statusText": resp.Status,
				"url":        resp.Request.URL.String(),
				"headers":    headerMap,
				"body":       string(bodyBytes),
				"error":      nil,
			}

			if col != nil {
				col.Add(ExecLogEntry{
					Timestamp: time.Now().UTC(),
					Kind:      "httpFetch",
					Name:      "httpFetch",
					Meta: map[string]any{
						"url":        result["url"],
						"status":     result["status"],
						"statusText": result["statusText"],
						"ok":         result["ok"],
						"phase":      "response",
					},
				})
			}

			return result, nil
		})
	})
}
