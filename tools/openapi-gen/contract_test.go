package main

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/contenox/contenox/tools/openapi-gen/internal/project"
)

func TestOSSRouteFilesUseApiframeworkOpenAPIContract(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	pkgs, err := project.LoadPackages(fset, root)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}

	var violations []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if !isOSSRouteFile(path) {
				continue
			}

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				if idx, ok := call.Fun.(*ast.IndexExpr); ok {
					if sel, ok := idx.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Decode" {
						comment := followingComment(call, file)
						if !strings.HasPrefix(comment, "@request ") {
							violations = append(violations, path+": Decode call missing // @request pkg.Type annotation")
						}
					}
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				switch sel.Sel.Name {
				case "Encode":
					comment := followingComment(call, file)
					if !strings.HasPrefix(comment, "@response ") {
						violations = append(violations, path+": Encode call missing // @response pkg.Type annotation")
					}
				case "GetPathParam":
					if !hasNonEmptyStringArg(call.Args, 2) {
						violations = append(violations, path+": GetPathParam requires a non-empty description")
					}
				case "GetQueryParam":
					if !hasNonEmptyStringArg(call.Args, 3) {
						violations = append(violations, path+": GetQueryParam requires a non-empty description")
					}
				}

				return true
			})
		}
	}

	if len(violations) > 0 {
		t.Fatalf("route contract violations:\n%s", strings.Join(violations, "\n"))
	}
}

func isOSSRouteFile(path string) bool {
	clean := filepath.ToSlash(path)
	base := filepath.Base(clean)
	if strings.HasSuffix(base, "_test.go") {
		return false
	}
	if strings.HasPrefix(clean, "/") {
		// keep normalized absolute path
	}
	if strings.Contains(clean, "/internal/") && strings.Contains(base, "route") {
		return true
	}
	return strings.Contains(clean, "/serverapi/") && strings.HasSuffix(base, "_routes.go")
}

func followingComment(node ast.Node, file *ast.File) string {
	pos := node.End()
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment.Slash == pos+1 || comment.Slash == pos+2 || comment.Slash == pos+3 {
				text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
				if strings.HasPrefix(text, "@") {
					return text
				}
			}
		}
	}
	return ""
}

func hasNonEmptyStringArg(args []ast.Expr, index int) bool {
	if len(args) <= index {
		return false
	}
	lit, ok := args[index].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	return strings.TrimSpace(strings.Trim(lit.Value, `"`)) != ""
}
