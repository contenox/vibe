package libacp_test

import (
	"reflect"
	"testing"

	"github.com/contenox/contenox/libacp"
)

func TestFlattenContent(t *testing.T) {
	tests := []struct {
		name        string
		blocks      []libacp.ContentBlock
		wantText    string
		wantDropped []string
	}{
		{
			name:     "nil blocks",
			blocks:   nil,
			wantText: "",
		},
		{
			name:     "empty slice",
			blocks:   []libacp.ContentBlock{},
			wantText: "",
		},
		{
			name:     "single text",
			blocks:   []libacp.ContentBlock{libacp.NewTextContent("hello")},
			wantText: "hello",
		},
		{
			name: "multiple text blocks join with newline",
			blocks: []libacp.ContentBlock{
				libacp.NewTextContent("a"),
				libacp.NewTextContent("b"),
			},
			wantText: "a\nb",
		},
		{
			name: "empty text contributes nothing and is not reported dropped",
			blocks: []libacp.ContentBlock{
				libacp.NewTextContent(""),
				libacp.NewTextContent("a"),
				libacp.NewTextContent(""),
				libacp.NewTextContent("b"),
			},
			wantText: "a\nb",
		},
		{
			name: "resource with inline text is inlined",
			blocks: []libacp.ContentBlock{
				libacp.NewResourceContent(libacp.EmbeddedResource{URI: "file:///x", Text: "body"}),
			},
			wantText: "body",
		},
		{
			name: "resource with only a blob is dropped",
			blocks: []libacp.ContentBlock{
				libacp.NewResourceContent(libacp.EmbeddedResource{URI: "file:///x", Blob: "AAAA"}),
			},
			wantText:    "",
			wantDropped: []string{"resource"},
		},
		{
			name:        "resource block with nil resource is dropped",
			blocks:      []libacp.ContentBlock{{Type: string(libacp.ContentKindResource)}},
			wantText:    "",
			wantDropped: []string{"resource"},
		},
		{
			name:     "resource link with name and uri",
			blocks:   []libacp.ContentBlock{libacp.NewResourceLink("file:///a.go", "a.go")},
			wantText: "a.go: file:///a.go",
		},
		{
			name:     "resource link with uri only",
			blocks:   []libacp.ContentBlock{libacp.NewResourceLink("file:///a.go", "")},
			wantText: "file:///a.go",
		},
		{
			name:     "resource link with name only",
			blocks:   []libacp.ContentBlock{libacp.NewResourceLink("", "a.go")},
			wantText: "a.go",
		},
		{
			name:     "resource link fields are trimmed",
			blocks:   []libacp.ContentBlock{libacp.NewResourceLink("  file:///a.go\t", "  a.go  ")},
			wantText: "a.go: file:///a.go",
		},
		{
			name:        "resource link with whitespace-only fields is dropped",
			blocks:      []libacp.ContentBlock{libacp.NewResourceLink("   ", "\t")},
			wantText:    "",
			wantDropped: []string{"resource_link"},
		},
		{
			name:        "image is dropped",
			blocks:      []libacp.ContentBlock{libacp.NewImageContent("AAAA", "image/png")},
			wantText:    "",
			wantDropped: []string{"image"},
		},
		{
			name:        "unknown type is dropped under its own name",
			blocks:      []libacp.ContentBlock{{Type: "video"}},
			wantText:    "",
			wantDropped: []string{"video"},
		},
		{
			name: "dropped kinds are deduplicated in first-seen order",
			blocks: []libacp.ContentBlock{
				libacp.NewImageContent("A", "image/png"),
				{Type: string(libacp.ContentKindAudio)},
				libacp.NewImageContent("B", "image/png"),
				{Type: string(libacp.ContentKindAudio)},
			},
			wantText:    "",
			wantDropped: []string{"image", "audio"},
		},
		{
			name: "mixed content keeps text order and reports drops",
			blocks: []libacp.ContentBlock{
				libacp.NewTextContent("intro"),
				libacp.NewImageContent("A", "image/png"),
				libacp.NewResourceLink("file:///a.go", "a.go"),
				libacp.NewResourceContent(libacp.EmbeddedResource{URI: "file:///b", Text: "b body"}),
				libacp.NewResourceContent(libacp.EmbeddedResource{URI: "file:///c", Blob: "AAAA"}),
				libacp.NewTextContent("outro"),
			},
			wantText:    "intro\na.go: file:///a.go\nb body\noutro",
			wantDropped: []string{"image", "resource"},
		},
		{
			name:     "empty type string is treated as unknown",
			blocks:   []libacp.ContentBlock{{Type: "", Text: "ignored"}},
			wantText: "",
			// The zero-value block reports "" as the dropped kind; the caller
			// still learns something was lost.
			wantDropped: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotDropped := libacp.FlattenContent(tt.blocks)
			if gotText != tt.wantText {
				t.Errorf("text = %q, want %q", gotText, tt.wantText)
			}
			if !reflect.DeepEqual(gotDropped, tt.wantDropped) {
				t.Errorf("dropped = %#v, want %#v", gotDropped, tt.wantDropped)
			}
		})
	}
}

// FlattenContent must not mutate the blocks it walks; callers reuse them.
func TestFlattenContentDoesNotMutateInput(t *testing.T) {
	blocks := []libacp.ContentBlock{
		libacp.NewTextContent("a"),
		libacp.NewResourceLink("  file:///a.go  ", "  a.go  "),
	}
	before := append([]libacp.ContentBlock(nil), blocks...)
	libacp.FlattenContent(blocks)
	if !reflect.DeepEqual(blocks, before) {
		t.Fatalf("input mutated: %#v", blocks)
	}
}
