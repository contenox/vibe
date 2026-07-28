package contenoxcli

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/contenox/contenox/internal/kernel/taskengine"
)

// maxAttachmentBytes bounds a single --attach file. Providers cap image inputs
// well below this; the limit exists so a mistyped path to a video or archive
// fails with a clear message instead of ballooning the request.
const maxAttachmentBytes = 10 << 20 // 10 MiB

// loadImageAttachments reads each --attach path into the ImagePart shape the
// user message carries (and every provider codec consumes). Content type is
// sniffed from the bytes, not trusted from the extension; anything that does
// not sniff as an image is rejected — attaching a non-image is always a
// mistake worth stopping, never something to hand the model silently.
func loadImageAttachments(paths []string) ([]taskengine.ImagePart, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	images := make([]taskengine.ImagePart, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("--attach %s: %w", path, err)
		}
		if info.Size() > maxAttachmentBytes {
			return nil, fmt.Errorf("--attach %s: %d bytes exceeds the %d MiB attachment limit", path, info.Size(), maxAttachmentBytes>>20)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--attach %s: %w", path, err)
		}
		mimeType := http.DetectContentType(data)
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("--attach %s: detected %s, not an image (png, jpeg, gif, and webp attach; other files belong in the prompt or a tool)", path, mimeType)
		}
		images = append(images, taskengine.ImagePart{Data: data, MimeType: mimeType})
	}
	return images, nil
}
