package libacp

import "strings"

// FlattenContent projects a content block list down to a single string — the
// inverse of content.go's constructors — for a consumer that can only accept
// flat text (a prompt field, a log line, a title).
//
// Lossy: image, audio, blob resources, and unknown block types carry no text
// and are dropped; a resource block contributes only its inline Resource.Text,
// never its Blob. Blocks join with a single newline (empty pieces contribute
// nothing) and a resource link renders as "name: uri" — one rendering policy,
// not a canonical one; a caller needing a different shape should write its
// own walk.
//
// dropped is the deduplicated, first-seen-ordered list of block types that
// could not be represented, so a caller can report the loss instead of
// silently swallowing it.
func FlattenContent(blocks []ContentBlock) (text string, dropped []string) {
	var b strings.Builder
	seen := map[string]bool{}

	drop := func(kind string) {
		if !seen[kind] {
			seen[kind] = true
			dropped = append(dropped, kind)
		}
	}
	appendText := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}

	for _, block := range blocks {
		switch block.Type {
		case string(ContentKindText):
			appendText(block.Text)
		case string(ContentKindResource):
			if block.Resource != nil && block.Resource.Text != "" {
				appendText(block.Resource.Text)
			} else {
				drop(block.Type)
			}
		case string(ContentKindResourceLink):
			name := strings.TrimSpace(block.Name)
			uri := strings.TrimSpace(block.URI)
			switch {
			case name != "" && uri != "":
				appendText(name + ": " + uri)
			case uri != "":
				appendText(uri)
			case name != "":
				appendText(name)
			default:
				drop(block.Type)
			}
		default:
			drop(block.Type)
		}
	}
	return b.String(), dropped
}
