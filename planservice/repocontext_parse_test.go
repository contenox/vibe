package planservice

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contenox/contenox/planstore"
)

func TestParseRepoContextRaw(t *testing.T) {
	raw := `{
  "schema_version": 0,
  "repo_root": "/home/x/proj",
  "languages": ["go", "ts"],
  "entry_points": ["cmd/main.go"],
  "build_commands": ["go build ./..."],
  "test_commands": ["go test ./..."],
  "conventions": ["table-driven tests"],
  "key_files": [{"path": "internal/foo/foo.go", "role": "core"}],
  "caveats": ["enterprise dir uses different module"]
}`
	rc, err := parseRepoContextRaw(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.SchemaVersion != planstore.RepoContextSchemaVersion {
		t.Fatalf("schema version not normalized: %d", rc.SchemaVersion)
	}
	if rc.RepoRoot != "/home/x/proj" {
		t.Fatalf("repo_root: %q", rc.RepoRoot)
	}
	if len(rc.KeyFiles) != 1 || rc.KeyFiles[0].Role != "core" {
		t.Fatalf("key_files: %+v", rc.KeyFiles)
	}
	if rc.GeneratedAt.IsZero() {
		t.Fatalf("generated_at should default to now()")
	}
}

func TestParseRepoContextRaw_AcceptsCodeFence(t *testing.T) {
	raw := "```json\n{\"languages\": [\"go\"]}\n```"
	rc, err := parseRepoContextRaw(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Languages) != 1 || rc.Languages[0] != "go" {
		t.Fatalf("got %+v", rc.Languages)
	}
}

func TestParseRepoContextRaw_RejectsCorrupted(t *testing.T) {
	raw := "self.__next_f.push([1,\"" + strings.Repeat("\\\\", 200) + "\"])"
	_, err := parseRepoContextRaw(raw)
	if err == nil {
		t.Fatalf("expected rejection")
	}
}

func TestParseRepoContextRaw_RejectsEmpty(t *testing.T) {
	if _, err := parseRepoContextRaw("   "); err == nil {
		t.Fatalf("expected error")
	}
}

func TestParseRepoContextRaw_TooLarge(t *testing.T) {
	raw := "{\"caveats\":[\"" + strings.Repeat("a", maxRepoContextBytes+10) + "\"]}"
	if _, err := parseRepoContextRaw(raw); err == nil {
		t.Fatalf("expected size rejection")
	}
}

func TestValidateRepoContext_CapsSlices(t *testing.T) {
	rc := &planstore.RepoContext{
		Conventions: make([]string, maxRepoContextSliceItems+50),
	}
	for i := range rc.Conventions {
		rc.Conventions[i] = "x"
	}
	if err := validateRepoContext(rc); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(rc.Conventions) != maxRepoContextSliceItems {
		t.Fatalf("expected slice capped at %d, got %d", maxRepoContextSliceItems, len(rc.Conventions))
	}
}

func TestRenderRepoContextBlock(t *testing.T) {
	rc := planstore.RepoContext{
		RepoRoot:      "/home/x/proj",
		Languages:     []string{"go"},
		BuildCommands: []string{"go build ./..."},
		KeyFiles:      []planstore.KeyFileNote{{Path: "main.go", Role: "entry"}},
	}
	b, _ := json.Marshal(rc)
	rendered := renderRepoContextBlock(string(b))
	for _, want := range []string{"repo_root: /home/x/proj", "languages: go", "build: go build", "main.go — entry"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in:\n%s", want, rendered)
		}
	}
}

func TestRenderRepoContextBlock_EmptyOnInvalid(t *testing.T) {
	if got := renderRepoContextBlock("garbage"); got != "" {
		t.Fatalf("expected empty render on invalid input, got %q", got)
	}
	if got := renderRepoContextBlock(""); got != "" {
		t.Fatalf("expected empty render on empty input, got %q", got)
	}
}
