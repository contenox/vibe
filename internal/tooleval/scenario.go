package tooleval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Meta is a scenario's meta.json: the blueprint's {tool_family, required,
// max_iterations, tags}.
type Meta struct {
	ToolFamily    string   `json:"tool_family"`
	Required      bool     `json:"required"`
	MaxIterations int      `json:"max_iterations"`
	Tags          []string `json:"tags,omitempty"`
}

// Invariant asserts a scenario's INVARIANT after a run — the task got done and the
// hostile shape was left alone — reading the final workspace and the collected trace.
// It must NEVER assert an exact tool-call sequence (blueprint: "never an exact
// tool-call sequence — models differ in path"). It returns pass plus human-readable
// reasons (both on pass and fail, so a green cell still explains itself). A nil
// invariant registered for an id means measurement-only (guidance-ab): TaskPass stays
// nil and the report row is the deliverable.
type Invariant func(workspace string, res *RunResult) (pass bool, reasons []string)

// FixtureBuilder generates the on-disk shapes a scenario needs that cannot be
// committed as static files — a 50 MiB executable, escaping symlinks, non-UTF8 blobs
// (blueprint: "real incident shapes"). It runs AFTER the static fixture/ tree is
// copied into the workspace, so it can add to or sit beside those files.
type FixtureBuilder func(workspace string) error

var (
	invariants      = map[string]Invariant{}
	fixtureBuilders = map[string]FixtureBuilder{}
)

// RegisterInvariant binds an invariant func to a scenario id (its directory name).
// Registration is by id, in Go, so the "verify.sh" idea from the blueprint becomes
// ordinary Go-test discipline. Panics on a duplicate id — a registration collision is
// a programming error, caught at init.
func RegisterInvariant(id string, fn Invariant) {
	if _, ok := invariants[id]; ok {
		panic("tooleval: duplicate invariant registration for " + id)
	}
	invariants[id] = fn
}

// RegisterFixtureBuilder binds a synthetic-fixture builder to a scenario id.
func RegisterFixtureBuilder(id string, fn FixtureBuilder) {
	if _, ok := fixtureBuilders[id]; ok {
		panic("tooleval: duplicate fixture builder registration for " + id)
	}
	fixtureBuilders[id] = fn
}

// Scenario is one loaded scenario: its data (instruction, meta, static fixture dir)
// plus its code (invariant, fixture builder) resolved from the registries by id.
type Scenario struct {
	ID           string
	Dir          string
	Instruction  string
	Meta         Meta
	Invariant    Invariant      // nil => measurement-only
	BuildFixture FixtureBuilder // nil => only the static tree is materialized
}

// scenariosRoot resolves the scenarios/ directory relative to THIS source file, so
// the harness finds its data whether tests run from the package dir or the repo root.
func scenariosRoot() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("tooleval: cannot resolve source location")
	}
	root := filepath.Join(filepath.Dir(self), "scenarios")
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("tooleval: scenarios dir not found at %s: %w", root, err)
	}
	return root, nil
}

// LoadScenario loads one scenario directory (instruction.md + meta.json) and resolves
// its registered invariant and fixture builder by the directory's base name.
func LoadScenario(dir string) (*Scenario, error) {
	id := filepath.Base(dir)

	instrBytes, err := os.ReadFile(filepath.Join(dir, "instruction.md"))
	if err != nil {
		return nil, fmt.Errorf("tooleval: scenario %s: instruction.md: %w", id, err)
	}
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("tooleval: scenario %s: meta.json: %w", id, err)
	}
	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("tooleval: scenario %s: meta.json: %w", id, err)
	}
	if meta.MaxIterations <= 0 {
		return nil, fmt.Errorf("tooleval: scenario %s: meta.json max_iterations must be > 0", id)
	}
	return &Scenario{
		ID:           id,
		Dir:          dir,
		Instruction:  string(instrBytes),
		Meta:         meta,
		Invariant:    invariants[id],
		BuildFixture: fixtureBuilders[id],
	}, nil
}

// LoadAll loads every scenario directory under scenarios/, sorted by id.
func LoadAll() ([]*Scenario, error) {
	root, err := scenariosRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*Scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := LoadScenario(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// LoadScenarioByID loads a single scenario by its id.
func LoadScenarioByID(id string) (*Scenario, error) {
	root, err := scenariosRoot()
	if err != nil {
		return nil, err
	}
	return LoadScenario(filepath.Join(root, id))
}

// Materialize copies the scenario's static fixture/ tree into a fresh workspace under
// root, then runs its fixture builder (if any) for the hostile synthetic shapes.
// Returns the workspace path. Each run gets its own workspace so nothing leaks across
// runs or arms.
func (s *Scenario) Materialize(root string) (string, error) {
	ws := filepath.Join(root, s.ID)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", err
	}
	fixtureDir := filepath.Join(s.Dir, "fixture")
	if _, err := os.Stat(fixtureDir); err == nil {
		if err := copyTree(fixtureDir, ws); err != nil {
			return "", fmt.Errorf("tooleval: materialize %s static fixture: %w", s.ID, err)
		}
	}
	if s.BuildFixture != nil {
		if err := s.BuildFixture(ws); err != nil {
			return "", fmt.Errorf("tooleval: materialize %s synthetic fixture: %w", s.ID, err)
		}
	}
	return ws, nil
}

// copyTree recursively copies src into dst (dst must exist). Regular files and
// directories only; the synthetic hostile shapes (symlinks, huge/binary files) are
// the fixture builder's job, kept out of the committed tree on purpose.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
