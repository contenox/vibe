// Package project loads parsed Go packages for the main module (go list ./...).
package project

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LoadPackages runs `go list -f '{{.Dir}}' ./...` from rootDir and parses all non-test .go files
// in those directories into ast.Package map keyed by package name (same merge semantics as before).
func LoadPackages(fset *token.FileSet, rootDir string) (map[string]*ast.Package, error) {
	pkgs := make(map[string]*ast.Package)

	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	cmd.Dir = rootDir
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go list: %w\n%s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("go list: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dir := strings.TrimSpace(line)
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("skip dir %s: %v", dir, err)
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			log.Printf("Found Go file: %s", path)

			fileAST, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				log.Printf("Error parsing file %s: %v", path, err)
				continue
			}

			pkgName := fileAST.Name.Name
			if pkgs[pkgName] == nil {
				pkgs[pkgName] = &ast.Package{
					Name:  pkgName,
					Files: make(map[string]*ast.File),
				}
			}
			pkgs[pkgName].Files[path] = fileAST
		}
	}
	return pkgs, nil
}
