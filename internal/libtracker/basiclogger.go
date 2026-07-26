package libtracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var _ ActivityTracker = (*logActivityTracker)(nil)

const (
	logChangeDataMaxJSONBytes = 32 * 1024
	logChangeDataPreviewBytes = 4 * 1024
)

// logActivityTracker is a simple implementation of ActivityTracker that logs events using slog.
type logActivityTracker struct {
	logger   *slog.Logger
	redactor *fieldRedactor
}

// LogOption customizes a log-backed ActivityTracker.
type LogOption func(*logActivityTracker)

// WithRedactedFields REPLACES the built-in sensitive field-name list (see
// DefaultRedactedFields) used to scrub values before they are logged. Names are
// matched case-insensitively as substrings, so "key" would scrub "api_key" and
// "keyring" alike. Passing no names disables redaction entirely, which is only
// appropriate where the caller can prove no credential ever reaches the tracker.
func WithRedactedFields(names ...string) LogOption {
	return func(t *logActivityTracker) { t.redactor = newFieldRedactor(names) }
}

// NewTextActivityTracker returns an ActivityTracker that emits structured text
// logs to w at Info level. It owns its slog wiring so callers (e.g. command
// entrypoints) don't have to import log/slog just to obtain a tracker.
func NewTextActivityTracker(w io.Writer, opts ...LogOption) ActivityTracker {
	return NewLogActivityTracker(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})), opts...)
}

// NewLogActivityTracker creates a new instance of logActivityTracker.
//
// Values are scrubbed by field name before being logged (see redact.go): this
// tracker is the shared instrumentation point for packages that handle tokens
// and keys, so redaction is on by default rather than something each caller has
// to remember.
func NewLogActivityTracker(logger *slog.Logger, opts ...LogOption) ActivityTracker {
	if logger == nil {
		logger = slog.Default()
	}
	t := &logActivityTracker{
		logger:   logger,
		redactor: newFieldRedactor(defaultRedactedFields),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// opSeq disambiguates operations that start within the same millisecond. The
// timestamp alone collides under any concurrency at all, which silently merges
// two unrelated operations' log lines under a single op_id and destroys the
// correlation the field exists for. The counter is process-local; across
// processes request_id/trace_id remain the correlation keys.
var opSeq atomic.Uint64

// Start implements the ActivityTracker interface.
func (t *logActivityTracker) Start(
	ctx context.Context,
	operation string,
	subject string,
	kvArgs ...any,
) (reportErr func(error), reportChange func(string, any), end func()) {
	startTime := time.Now()

	// Generate an operation ID: timestamp for readability, counter for uniqueness.
	opID := "op-" + formatTimestamp(startTime) + "-" + strconv.FormatUint(opSeq.Add(1), 36)
	attrs := []slog.Attr{
		slog.String("operation", operation),
		slog.String("subject", subject),
		slog.String("op_id", opID),
	}
	requestID := ""
	if val, ok := ctx.Value(ContextKeyRequestID).(string); ok {
		requestID = val
	}
	traceID := ""
	if val, ok := ctx.Value(ContextKeyTraceID).(string); ok {
		traceID = val
	}
	spanID := ""
	if val, ok := ctx.Value(ContextKeySpanID).(string); ok {
		spanID = val
	}
	if len(requestID) > 0 {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if len(traceID) > 0 {
		attrs = append(attrs, slog.String("trace_id", traceID))
	}
	if len(spanID) > 0 {
		attrs = append(attrs, slog.String("span_id", spanID))
	}
	arrs := append(attrs, toSlogAttrs(t.redactor, kvArgs...)...)
	// Initial log entry: start of the operation
	t.logger.LogAttrs(ctx, slog.LevelInfo, "Operation started",
		arrs...,
	)

	// Return functions to be used by caller

	reportErrFunc := func(err error) {
		if err != nil {
			attr := []any{
				slog.String("operation", operation),
				slog.String("subject", subject),
				slog.String("op_id", opID),
				slog.Any("error", err),
			}
			if len(requestID) > 0 {
				attr = append(attr, slog.String("request_id", requestID))
			}
			if len(traceID) > 0 {
				attr = append(attr, slog.String("trace_id", traceID))
			}
			if len(spanID) > 0 {
				attr = append(attr, slog.String("span_id", spanID))
			}
			t.logger.ErrorContext(ctx, "Operation failed", attr...)
		}
	}

	reportChangeFunc := func(id string, data any) {
		attr := []slog.Attr{
			slog.String("operation", operation),
			slog.String("subject", subject),
			slog.String("op_id", opID),
			slog.String("change_id", id),
			slog.Any("change_data", boundedLogValue(data, t.redactor)),
		}
		if len(spanID) > 0 {
			attr = append(attr, slog.String("span_id", spanID))
		}
		if len(traceID) > 0 {
			attr = append(attr, slog.String("trace_id", traceID))
		}
		if len(requestID) > 0 {
			attr = append(attr, slog.String("request_id", requestID))
		}
		t.logger.LogAttrs(ctx, slog.LevelInfo, "State changed", attr...)
	}

	endFunc := func() {
		duration := time.Since(startTime)
		attr := []slog.Attr{
			slog.String("operation", operation),
			slog.String("subject", subject),
			slog.String("op_id", opID),
			slog.Duration("duration", duration),
		}
		if len(spanID) > 0 {
			attr = append(attr, slog.String("span_id", spanID))
		}
		if len(traceID) > 0 {
			attr = append(attr, slog.String("trace_id", traceID))
		}
		if len(requestID) > 0 {
			attr = append(attr, slog.String("request_id", requestID))
		}
		t.logger.LogAttrs(ctx, slog.LevelInfo, "Operation completed", attr...)
	}

	return reportErrFunc, reportChangeFunc, endFunc
}

// boundedLogValue prepares an arbitrary payload for logging: it scrubs
// credential-looking fields, then caps the result so one oversized change never
// floods the log.
//
// Redaction happens BEFORE the size cap so the truncated summary — preview and
// sha256 alike — is derived from already-scrubbed bytes; otherwise a secret
// could survive in the preview of a payload that was too big to log in full.
func boundedLogValue(v any, red *fieldRedactor) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{
			"truncated":     true,
			"reason":        "json_marshal_error: " + capLogString(err.Error(), 512),
			"original_type": logTypeName(v),
		}
	}
	if scrubbed, changed := red.redactMarshaled(raw, v); changed {
		v = scrubbed
		if b, mErr := json.Marshal(scrubbed); mErr == nil {
			raw = b
		}
	}
	if len(raw) <= logChangeDataMaxJSONBytes {
		return v
	}
	sum := sha256.Sum256(raw)
	preview := previewLogBytes(raw, logChangeDataPreviewBytes)
	return map[string]any{
		"truncated":           true,
		"reason":              "payload_exceeds_limit",
		"original_type":       logTypeName(v),
		"original_json_bytes": len(raw),
		"sha256":              hex.EncodeToString(sum[:]),
		"preview":             preview,
		"preview_bytes":       len([]byte(preview)),
	}
}

func capLogString(s string, maxBytes int) string {
	if maxBytes <= 0 || len([]byte(s)) <= maxBytes {
		return s
	}
	return previewLogBytes([]byte(s), maxBytes)
}

func previewLogBytes(raw []byte, maxBytes int) string {
	if maxBytes > 0 && len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	return strings.ToValidUTF8(string(raw), "?")
}

func logTypeName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return fmt.Sprintf("%T", v)
	}
	return t.String()
}

// Helper: Convert key-value pairs into slog attributes.
//
// kvArgs is the same leak path as change_data — callers pass "api_key", value
// just as readily — so it gets the same scrubbing, keyed on the attribute name
// as well as on any nested field names inside the value.
func toSlogAttrs(red *fieldRedactor, kvArgs ...any) []slog.Attr {
	var attrs []slog.Attr
	for i := 0; i < len(kvArgs); i += 2 {
		key, ok := kvArgs[i].(string)
		if !ok || i+1 >= len(kvArgs) {
			continue
		}
		if red.sensitive(key) {
			attrs = append(attrs, slog.String(key, redactedPlaceholder))
			continue
		}
		val, _ := red.redactValue(kvArgs[i+1])
		attrs = append(attrs, slog.Any(key, val))
	}
	return attrs
}

// Helper: Format timestamp for op_id
func formatTimestamp(t time.Time) string {
	return t.Format("20060102-150405.000")
}

type contextKey string

func (c contextKey) String() string {
	return string(c)
}
