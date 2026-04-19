package planstore

import "time"

// RepoContextSchemaVersion is the current schema version for [RepoContext]
// JSON documents persisted on a plan. Bump when the shape changes; readers
// should tolerate older versions or ignore unknown fields.
const RepoContextSchemaVersion = 1

// RepoContext is a typed snapshot of a workspace's high-signal facts produced
// by the explorer chain (chain-plan-explorer.json) and persisted on
// [Plan.RepoContextJSON]. It is rendered into the seed prompt as
// {{var:repo_context}} so each plan step starts with concrete file paths,
// build / test commands, and conventions instead of cold exploration.
//
// Fields are intentionally generic — the explorer model fills only what it
// can verify with read-only tools (filesystem grep / read). Any field may be
// empty; consumers must not rely on any particular field being populated.
type RepoContext struct {
	SchemaVersion int           `json:"schema_version"`
	RepoRoot      string        `json:"repo_root,omitempty"`
	Languages     []string      `json:"languages,omitempty"`
	EntryPoints   []string      `json:"entry_points,omitempty"`
	BuildCommands []string      `json:"build_commands,omitempty"`
	TestCommands  []string      `json:"test_commands,omitempty"`
	Conventions   []string      `json:"conventions,omitempty"`
	KeyFiles      []KeyFileNote `json:"key_files,omitempty"`
	Caveats       []string      `json:"caveats,omitempty"`
	GeneratedAt   time.Time     `json:"generated_at,omitempty"`
}

// KeyFileNote attaches a short role description to a notable path, e.g.
// {Path: "runtime/contenoxcli/cli.go", Role: "CLI entrypoint"}.
type KeyFileNote struct {
	Path string `json:"path"`
	Role string `json:"role,omitempty"`
}
