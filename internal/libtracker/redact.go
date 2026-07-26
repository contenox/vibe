package libtracker

import (
	"encoding/json"
	"strings"
)

// redactedPlaceholder replaces a sensitive value. The field NAME is deliberately
// kept: the shape of the log entry stays readable and greppable ("a token was
// set here"), while the thing that must never reach a log file — the value — is
// gone.
const redactedPlaceholder = "[REDACTED]"

// maxRedactDepth bounds the walk over a decoded payload. A log payload nested
// this deeply is pathological, so anything below the limit is collapsed rather
// than passed through: a credential must not escape merely by being buried deep
// enough to outrun the scrubber.
const maxRedactDepth = 64

// defaultRedactedFields are matched as case-insensitive SUBSTRINGS of a
// normalized field name, so "user_password", "X-Api-Key" and "AuthorizationHeader"
// are all caught without enumerating every spelling a caller might invent.
//
// The list errs on the side of over-redaction: a wrongly scrubbed debugging
// field costs one confused developer, a leaked credential costs a rotation.
// Callers that need a different trade-off can replace it via WithRedactedFields.
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
	// Explicit token spellings, listed ahead of the bare "token" rule below so
	// they are never silently exempted by the accounting-field carve-out.
	"api_token",
	"auth_token",
	"access_token",
	"refresh_token",
	"id_token",
	"bearer",
	// Bare "token" catches everything else; see tokenAccountingFields.
	"token",
}

// tokenAccountingFields exempts the bare "token" rule. This repo instruments
// LLM work, so "max_tokens", "prompt_tokens" and "token_count" are ordinary,
// useful telemetry — redacting them would make the tracker useless for the
// workload it mostly observes. Only the bare "token" rule consults this list,
// so "auth_tokens" still matches the explicit "auth_token" entry above.
var tokenAccountingFields = []string{
	"tokens",
	"token_count",
	"tokenizer",
	"tokenized",
}

// DefaultRedactedFields returns a copy of the built-in sensitive field-name
// list. It exists so a caller can extend rather than replace the defaults:
//
//	WithRedactedFields(append(DefaultRedactedFields(), "ssn")...)
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

// normalizeFieldName folds the spellings a field name arrives in — JSON tags,
// Go field names, HTTP headers — onto one form so a single substring rule
// covers "apiKey", "api-key", "API Key" and "api_key".
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
// was actually changed.
//
// It works on the JSON projection of v rather than on v itself: that is the
// same view the log handler will render, it applies uniformly to structs, maps
// and slices, and the decoded tree is acyclic by construction — so a struct
// graph with cycles fails at Marshal (handled by the caller) instead of hanging
// the scrubber.
//
// When nothing matched it returns the ORIGINAL value, so payloads without
// secrets are logged byte-for-byte as they were before redaction existed.
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
		// Fail closed: below the depth limit we stop being able to vouch for
		// the contents, so we drop them rather than emit them unchecked.
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
		// Scalars carry no field name of their own; their key decided already.
		return v, false
	}
}
