package agentdecl

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed preseed/reviewer.md
var preseedReviewer string

//go:embed preseed/researcher.md
var preseedResearcher string

//go:embed preseed/README.md
var preseedReadme string

// preseedTrees is the worked example of the DIRECTORY convention: a router and
// the branches it chooses between. Embedded as a filesystem rather than as
// named strings because the shape is the point — a flat list of files would
// have to re-encode the nesting that the directories already state.
//
//go:embed preseed/agents
var preseedTrees embed.FS

// Preseeded are the files that establish the authoring convention.
var Preseeded = []struct {
	// RelPath is relative to the contenox directory.
	RelPath string
	Content func() string
}{
	{ConfigFilename, func() string { return string(shippedConfig) }},
	{filepath.Join(NativeSourceDir, "README.md"), func() string { return preseedReadme }},
	{filepath.Join(NativeSourceDir, "reviewer.md"), func() string { return preseedReviewer }},
	{filepath.Join(NativeSourceDir, "researcher.md"), func() string { return preseedResearcher }},
}

// Preseed writes the authoring convention into contenoxDir, leaving existing
// files alone, and returns the paths it created. The defaults file is written
// verbatim so its comments survive: they are what makes the knobs tunable.
func Preseed(contenoxDir string) ([]string, error) {
	if contenoxDir == "" {
		return nil, nil
	}
	var created []string
	for _, f := range Preseeded {
		path := filepath.Join(contenoxDir, f.RelPath)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return created, fmt.Errorf("agentdecl: create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(f.Content()), 0o644); err != nil {
			return created, fmt.Errorf("agentdecl: write %s: %w", path, err)
		}
		created = append(created, path)
	}

	// The tree example, written with its directories intact. Skipped file by
	// file rather than wholesale, so an operator who deleted one branch does not
	// get it back while an operator who edited another keeps their edit.
	err := fs.WalkDir(preseedTrees, "preseed/agents", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, relErr := filepath.Rel("preseed/agents", p)
		if relErr != nil {
			return relErr
		}
		path := filepath.Join(contenoxDir, NativeSourceDir, filepath.FromSlash(rel))
		if _, sErr := os.Stat(path); sErr == nil {
			return nil
		}
		data, rErr := preseedTrees.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		if mErr := os.MkdirAll(filepath.Dir(path), 0o750); mErr != nil {
			return fmt.Errorf("agentdecl: create %s: %w", filepath.Dir(path), mErr)
		}
		if wErr := os.WriteFile(path, data, 0o644); wErr != nil {
			return fmt.Errorf("agentdecl: write %s: %w", path, wErr)
		}
		created = append(created, path)
		return nil
	})
	if err != nil {
		return created, err
	}
	return created, nil
}
