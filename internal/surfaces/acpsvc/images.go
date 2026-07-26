package acpsvc

import (
	"encoding/base64"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/libacp"
)

// extractImageParts splits a prompt's content blocks into the image attachments
// and everything else. Image blocks become taskengine.ImagePart (the shape the
// chain's user Message and every provider codec consume); the remaining blocks
// go on to libacp.FlattenContent, whose documented lossy text projection would
// otherwise DROP images silently — this extraction is what makes vision reach
// the model instead of the telemetry log.
//
// An image block whose Data is not valid base64 (the ACP wire encoding) is not
// an attachment we can send; it is returned in rest so FlattenContent counts it
// among the dropped kinds and the degradation stays visible, exactly like an
// unsupported block type.
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
