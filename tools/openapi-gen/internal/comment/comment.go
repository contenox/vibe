// Package comment extracts documentation text from go/ast comment groups for OpenAPI generation.
package comment

import (
	"go/ast"
	"strings"
)

// Text returns cleaned documentation from a comment group.
func Text(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}

	comments := make([]string, 0, len(doc.List))
	for _, c := range doc.List {
		text := c.Text

		switch {
		case strings.HasPrefix(text, "//"):
			text = strings.TrimPrefix(text, "//")
		case strings.HasPrefix(text, "/*"):
			text = strings.TrimPrefix(text, "/*")
			text = strings.TrimSuffix(text, "*/")
		}

		text = strings.TrimSpace(text)
		if text != "" {
			comments = append(comments, text)
		}
	}
	return strings.Join(comments, "\n")
}
