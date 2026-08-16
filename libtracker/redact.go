package libtracker

import (
	"encoding/json"
	"strings"
)

// redactedPlaceholder replaces a sensitive value while keeping the field name.
const redactedPlaceholder = "[REDACTED]"

// maxRedactDepth bounds the walk over a decoded payload.
const maxRedactDepth = 64

// defaultRedactedFields are matched as case-insensitive substrings of a
// normalized field name.
var defaultRedactedFields = []string{
	"password",
	"passwd",
	"passphrase",
	"secret",
	"credential",
	"authorization",
	"private_key",
	"privatekey",
	"api_key",
	"apikey",
	"access_key",
	"accesskey",
	"signing_key",
	"session_id",
	"sessionid",
	"jwt",
	// Explicit token spellings, exempt from the accounting-field carve-out.
	"api_token",
	"auth_token",
	"access_token",
	"refresh_token",
	"id_token",
	"bearer",
	// Bare "token" catches everything else; see tokenAccountingFields.
	"token",
}

// tokenAccountingFields exempts the bare "token" rule so LLM telemetry like
// "max_tokens" isn't redacted.
var tokenAccountingFields = []string{
	"tokens",
	"token_count",
	"tokenizer",
	"tokenized",
}

// DefaultRedactedFields returns a copy of the built-in sensitive field-name
// list, so a caller can extend rather than replace the defaults.
func DefaultRedactedFields() []string {
	out := make([]string, len(defaultRedactedFields))
	copy(out, defaultRedactedFields)
	return out
}

// fieldRedactor scrubs values whose field name looks sensitive.
type fieldRedactor struct {
	fields []string
}

func newFieldRedactor(fields []string) *fieldRedactor {
	norm := make([]string, 0, len(fields))
	for _, f := range fields {
		if n := normalizeFieldName(f); n != "" {
			norm = append(norm, n)
		}
	}
	return &fieldRedactor{fields: norm}
}

// normalizeFieldName folds JSON tags, Go field names and HTTP headers onto one
// form so a single substring rule covers every spelling.
func normalizeFieldName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return name
}

// sensitive reports whether a field with this name must have its value scrubbed.
func (r *fieldRedactor) sensitive(name string) bool {
	if r == nil {
		return false
	}
	n := normalizeFieldName(name)
	if n == "" {
		return false
	}
	for _, f := range r.fields {
		if !strings.Contains(n, f) {
			continue
		}
		if f == "token" && isTokenAccountingField(n) {
			continue
		}
		return true
	}
	return false
}

func isTokenAccountingField(normalized string) bool {
	for _, exempt := range tokenAccountingFields {
		if strings.Contains(normalized, exempt) {
			return true
		}
	}
	return false
}

// redactValue returns v with sensitive fields scrubbed, plus whether anything
// changed. It works on the JSON projection of v, not v itself.
func (r *fieldRedactor) redactValue(v any) (any, bool) {
	if r == nil || v == nil {
		return v, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, false
	}
	return r.redactMarshaled(raw, v)
}

// redactMarshaled is redactValue for callers that already hold v's JSON bytes.
func (r *fieldRedactor) redactMarshaled(raw []byte, v any) (any, bool) {
	if r == nil {
		return v, false
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return v, false
	}
	out, changed := r.redactTree(tree, 0)
	if !changed {
		return v, false
	}
	return out, true
}

func (r *fieldRedactor) redactTree(v any, depth int) (any, bool) {
	if depth > maxRedactDepth {
		// Fail closed below the depth limit.
		return redactedPlaceholder, true
	}
	switch node := v.(type) {
	case map[string]any:
		var changed bool
		out := make(map[string]any, len(node))
		for k, val := range node {
			if r.sensitive(k) {
				out[k] = redactedPlaceholder
				changed = true
				continue
			}
			nv, c := r.redactTree(val, depth+1)
			out[k] = nv
			changed = changed || c
		}
		if !changed {
			return v, false
		}
		return out, true
	case []any:
		var changed bool
		out := make([]any, len(node))
		for i, val := range node {
			nv, c := r.redactTree(val, depth+1)
			out[i] = nv
			changed = changed || c
		}
		if !changed {
			return v, false
		}
		return out, true
	default:
		return v, false
	}
}
