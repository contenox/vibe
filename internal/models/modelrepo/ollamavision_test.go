package modelrepo_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/contenox/contenox/internal/models/modelrepo/ollama"
	"github.com/ollama/ollama/api"
	"github.com/stretchr/testify/require"
)

// visionModel is the smallest broadly-available ollama vision model (~1.7GB).
const visionModel = "moondream"

// redCirclePNG renders a solid red circle on white — an image whose one
// unambiguous property a small vision model can be asked about.
func redCirclePNG(t *testing.T) []byte {
	t.Helper()
	const size = 256
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	center, radius := size/2, size/3
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-center, y-center
			if dx*dx+dy*dy <= radius*radius {
				img.Set(x, y, color.RGBA{R: 220, A: 255})
			} else {
				img.Set(x, y, color.White)
			}
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestSystem_Ollama_Vision(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ollama vision system test: starts a container and pulls a multi-GB vision model")
	}
	ctx := t.Context()
	uri, _, cleanup, err := modelrepo.SetupOllamaLocalInstance(ctx, "latest")
	require.NoError(t, err)
	defer cleanup()

	u, err := url.Parse(uri)
	require.NoError(t, err)
	ollamaClient := api.NewClient(u, http.DefaultClient)

	t.Logf("Pulling vision model: %s", visionModel)
	require.NoError(t, pullModel(t, ollamaClient, visionModel))
	require.NoError(t, waitForModelReady(t, ollamaClient, visionModel))

	caps := modelrepo.CapabilityConfig{
		ContextLength: 2048,
		CanChat:       true,
		CanVision:     true,
	}
	provider := ollama.NewOllamaProvider(visionModel, []string{uri}, http.DefaultClient, caps, "", nil)
	chatClient, err := provider.GetChatConnection(ctx, uri)
	require.NoError(t, err)

	resp, err := chatClient.Chat(ctx, []modelrepo.Message{
		{
			Role:    "user",
			Content: "What color is the shape in this image? Answer with the color name only.",
			Images:  []modelrepo.ImagePart{{Data: redCirclePNG(t), MimeType: "image/png"}},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Message.Content)
	require.Contains(t, strings.ToLower(resp.Message.Content), "red",
		"the model must actually SEE the image: a red circle answered without 'red' means the image never reached it")
}
