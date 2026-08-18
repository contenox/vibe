package agentdecl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/contenox/contenox/internal/services/hitlservice"
)

// RenderEnvelopePolicy transpiles one named envelope into the policy document
// the loader reads, annotated with where it came from so the file explains
// itself to whoever finds it.
func RenderEnvelopePolicy(cfg Config, name, source string) ([]byte, error) {
	env, err := cfg.ResolveEnvelope(name)
	if err != nil {
		return nil, err
	}
	out, err := TranspileEnvelope(env)
	if err != nil {
		return nil, err
	}
	if source == "" {
		source = ConfigFilename
	}
	origin := fmt.Sprintf("Rendered from [%s.%s] in %s. Derived and disposable: edit the envelope, not this file. A hand-written %s in .contenox/ or ~/.contenox/ shadows it.",
		EnvelopeSection, name, source, EnvelopePolicyFile(name))
	if env.Description != "" {
		origin = env.Description + " " + origin
	}
	annotations := [][2]any{{"//", origin}}
	if len(out.Notes) > 0 {
		annotations = append(annotations, [2]any{"//reserved", out.Notes})
	}
	return marshalPolicyDoc(out.Policy, annotations)
}

// SyncEnvelopePolicy writes one rendered envelope into generatedDir, reporting
// whether the file changed. The directory is derived, so an unchanged render is
// left alone rather than rewritten.
func SyncEnvelopePolicy(cfg Config, name, generatedDir, source string) (string, bool, error) {
	raw, err := RenderEnvelopePolicy(cfg, name, source)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(generatedDir, EnvelopePolicyFile(name))
	if fileHas(path, raw) {
		return path, false, nil
	}
	if err := os.MkdirAll(generatedDir, 0o750); err != nil {
		return "", false, fmt.Errorf("agentdecl: create %s: %w", generatedDir, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", false, fmt.Errorf("agentdecl: write %s: %w", path, err)
	}
	return path, true, nil
}

// marshalPolicyDoc splices the schema and the annotations ahead of the policy
// body, so the keys that explain the file are the first thing read.
func marshalPolicyDoc(p *hitlservice.Policy, annotations [][2]any) ([]byte, error) {
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("{\n  \"$schema\": %q,\n", PolicySchemaURL)
	for _, ann := range annotations {
		value, err := json.Marshal(ann[1])
		if err != nil {
			return nil, err
		}
		head += fmt.Sprintf("  %q: %s,\n", ann[0], value)
	}
	if len(body) < 2 || body[0] != '{' || body[1] != '\n' {
		return body, nil
	}
	return append([]byte(head), body[2:]...), nil
}
