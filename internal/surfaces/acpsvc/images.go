package acpsvc

import (
	"encoding/base64"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/libacp"
)

// extractImageParts splits content blocks into image attachments (as
// taskengine.ImagePart, for CanVision providers) and the rest, which still go
// through libacp.FlattenContent — otherwise its lossy text projection would
// drop images silently. A block with invalid base64 data is returned in rest
// instead, so it surfaces as a dropped kind rather than a silent loss.
func extractImageParts(blocks []libacp.ContentBlock) (images []taskengine.ImagePart, rest []libacp.ContentBlock) {
	for _, block := range blocks {
		if block.Type != string(libacp.ContentKindImage) {
			rest = append(rest, block)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil || len(data) == 0 {
			rest = append(rest, block)
			continue
		}
		images = append(images, taskengine.ImagePart{Data: data, MimeType: block.MimeType})
	}
	return images, rest
}
